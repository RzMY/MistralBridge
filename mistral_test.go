package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTransformRequestMistralThinking(t *testing.T) {
	input := []byte(`{"model":"mistral-bridge/alias","reasoning_effort":"high","messages":[{"role":"user","content":"Question"},{"role":"assistant","content":"Earlier answer","reasoning_content":"Prior thought"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}],"reasoning_content":"Use the read tool"},{"role":"tool","tool_call_id":"call_1","content":"Done","reasoning_content":"discard"}]}`)
	got, err := transformRequest(input, "mistral-large-4-0", "high=medium")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model           string                       `json:"model"`
		ReasoningEffort string                       `json:"reasoning_effort"`
		Messages        []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "mistral-large-4-0" || body.ReasoningEffort != "medium" {
		t.Fatalf("model or reasoning level was not mapped: %s", got)
	}
	var first map[string]json.RawMessage
	if err := json.Unmarshal(body.Messages[1]["content"], &first); err == nil {
		t.Fatal("assistant content should be converted to typed parts")
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(body.Messages[1]["content"], &parts); err != nil || len(parts) != 2 {
		t.Fatalf("assistant content parts = %s (%v)", body.Messages[1]["content"], err)
	}
	if string(parts[0]["type"]) != `"thinking"` || string(parts[1]["type"]) != `"text"` {
		t.Fatalf("unexpected parts: %s", body.Messages[1]["content"])
	}
	if _, exists := body.Messages[1]["reasoning_content"]; exists {
		t.Fatal("reasoning_content should be removed")
	}
	if _, exists := body.Messages[2]["reasoning_content"]; exists {
		t.Fatal("tool call reasoning_content should be removed")
	}
	if content := string(body.Messages[2]["content"]); !strings.Contains(content, `"thinking"`) || !strings.Contains(content, `Use the read tool`) {
		t.Fatalf("tool call lost reasoning content: %s", content)
	}
	if content := string(body.Messages[3]["content"]); content != `"Done"` {
		t.Fatalf("tool result was changed: %s", content)
	}
}

func TestTransformRequestDropsBlankReasoning(t *testing.T) {
	got, err := transformRequest([]byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[],"reasoning_content":"  "}]}`), "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if _, exists := messages[0]["reasoning_content"]; exists {
		t.Fatal("blank reasoning placeholder should be removed")
	}
	if string(messages[0]["content"]) != "null" {
		t.Fatalf("empty assistant content changed: %s", messages[0]["content"])
	}
}

func TestNormalizeCompletionThinkingParts(t *testing.T) {
	input := []byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"Reasoning."}]},{"type":"text","text":"Answer."}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	got, err := normalizeCompletion(input)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "Answer." || body.Choices[0].Message.Reasoning != "Reasoning." {
		t.Fatalf("unexpected normalized response: %s", got)
	}
}

func TestNormalizeSSEThinkingParts(t *testing.T) {
	input := []byte(`data: {"choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"Think"}]},{"type":"text","text":"A"}]}}]}` + "\n")
	got, err := normalizeSSELine(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"reasoning_content":"Think"`) || !strings.Contains(string(got), `"content":"A"`) {
		t.Fatalf("unexpected normalized SSE line: %s", got)
	}
}

func TestChatCatalogFiltersUnsupportedModels(t *testing.T) {
	input := []byte(`{"data":[{"id":"chat-model","capabilities":{"completion_chat":true}},{"id":"embedding-model","capabilities":{"completion_chat":false}}]}`)
	models, err := parseChatCatalog(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "chat-model" {
		t.Fatalf("chat models = %#v", models)
	}
	models, err = parseChatCatalog([]byte(`{"data":[{"id":"legacy-model"}]}`))
	if err != nil || len(models) != 1 || models[0] != "legacy-model" {
		t.Fatalf("legacy catalog = %#v, %v", models, err)
	}
}
