package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRequestEditDefaultMigrationPersistenceAndRollback(t *testing.T) {
	isolatedState(t)
	path, _ := statePath()
	if err := os.WriteFile(path, []byte(`{"keys":[],"models":[],"catalog":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	stateLoaded = false
	if config := currentState().RequestEdit; !config.BuiltinEnabled || config.Rules == nil || len(config.Rules) != 0 {
		t.Fatalf("legacy state must enable builtins with no custom rules: %#v", config)
	}
	response := managementCall(t, "PATCH", "/request-edit", `{"rules":[{"name":"copy tag","enabled":true,"action":"copy","source":"header","from":"X-Client-Tag","target":"body","path":"/metadata/tag","overwrite":true}]}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	config := currentState().RequestEdit
	if config.Rules[0].ID == "" || !config.BuiltinEnabled {
		t.Fatal("saving rules must assign IDs and preserve the independent toggle")
	}
	response = managementCall(t, "PATCH", "/request-edit", `{"builtin_enabled":false}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	stateLoaded = false
	config = currentState().RequestEdit
	if config.BuiltinEnabled || len(config.Rules) != 1 {
		t.Fatal("disabled builtins and custom rules did not survive a reload")
	}
	// Snapshots must not share mutable slices or literal storage with live state.
	config.Rules[0].Name = "changed snapshot"
	if currentState().RequestEdit.Rules[0].Name != "copy tag" {
		t.Fatal("state snapshot aliases the rules")
	}
	before, _ := json.Marshal(currentState().RequestEdit)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISTRAL_BRIDGE_DATA_DIR", filepath.Join(blocker, "nested"))
	response = managementCall(t, "PATCH", "/request-edit", `{"builtin_enabled":true,"rules":[]}`)
	after, _ := json.Marshal(currentState().RequestEdit)
	if response.StatusCode != 500 || string(before) != string(after) {
		t.Fatal("failed save changed the active rule configuration")
	}
}

func TestRequestEditBuiltinAliasesNativePrecedenceAndIsolation(t *testing.T) {
	input := `{"model":"client","messages":[{"role":"developer","content":"system","name":"client"},{"role":"assistant","content":"answer","refusal":null,"reasoning_content":"thought"},{"role":"tool","name":"fn","content":"result","tool_call_id":"call"}],"max_tokens":64,"max_completion_tokens":128,"max_output_tokens":256,"seed":9007199254740993,"random_seed":null,"stop_sequences":["END"],"topP":0.8,"topK":12,"reasoning":{"effort":"high","summary":"auto"},"generationConfig":{"maxOutputTokens":512,"candidateCount":2},"stream_options":{"include_usage":true},"store":false,"user":"client-id","logit_bias":{},"prompt_cache_retention":"24h","safety_identifier":"id","parallel_tool_calls":false,"logprobs":true,"safe_prompt":true,"metadata":{"seed":123,"store":false},"tools":[{"type":"function","function":{"name":"fn","strict":true,"parameters":{"properties":{"seed":{"type":"integer"},"store":{"type":"boolean"}}}}}]}`
	body, headers, err := editCompletionRequest([]byte(input), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(body, &root)
	for field, want := range map[string]string{"model": `"client"`, "max_tokens": "64", "random_seed": "9007199254740993", "stop": `["END"]`, "top_p": "0.8", "top_k": "12", "reasoning_effort": `"high"`, "n": "2", "parallel_tool_calls": "false", "logprobs": "true", "safe_prompt": "true"} {
		if string(root[field]) != want {
			t.Errorf("%s = %s, want %s", field, root[field], want)
		}
	}
	for _, field := range []string{"max_completion_tokens", "max_output_tokens", "seed", "stop_sequences", "topP", "topK", "reasoning", "generationConfig", "stream_options", "store", "user", "logit_bias", "prompt_cache_retention", "safety_identifier"} {
		if _, exists := root[field]; exists {
			t.Errorf("client-only field %s survived native mapping", field)
		}
	}
	if string(root["metadata"]) != `{"seed":123,"store":false}` || !strings.Contains(string(root["tools"]), `"strict":true`) || !strings.Contains(string(root["tools"]), `"seed":{"type":"integer"}`) {
		t.Fatal("mapping rewrote unrelated metadata or tool schemas")
	}
	var messages []map[string]json.RawMessage
	_ = json.Unmarshal(root["messages"], &messages)
	if string(messages[0]["role"]) != `"system"` || messages[0]["name"] != nil || messages[1]["refusal"] != nil || !strings.Contains(string(messages[1]["content"]), `"type":"thinking"`) || string(messages[2]["name"]) != `"fn"` || len(headers) != 0 {
		t.Fatal("message mapping did not respect Mistral's message schemas")
	}
	// Disabling only the builtin mapper keeps client fields and message shapes.
	disabled := false
	body, _, err = editCompletionRequest([]byte(input), nil, &requestEditConfig{BuiltinEnabled: false, AutoDropIncompatible: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(body, &root)
	if string(root["seed"]) != "9007199254740993" || string(root["store"]) != "false" || !strings.Contains(string(root["messages"]), `"role":"developer"`) {
		t.Fatal("disabled builtins still modified client-specific fields")
	}
}

func TestRequestEditOrderedRulesWildcardsHeadersAndExactNumbers(t *testing.T) {
	config := &requestEditConfig{BuiltinEnabled: true, Rules: []requestEditRule{
		{Name: "default temperature", Enabled: true, Action: "set", Target: "body", Path: "/temperature", ValueJSON: "0.2"},
		{Name: "override temperature", Enabled: true, Action: "set", Target: "body", Path: "/temperature", ValueJSON: "0.7", Overwrite: true},
		{Name: "exact seed", Enabled: true, Action: "set", Target: "body", Path: "/random_seed", ValueJSON: "9007199254740993", Overwrite: true},
		{Name: "copy object", Enabled: true, Action: "copy", Source: "body", From: "/metadata/source", Target: "body", Path: "/metadata/copy", Overwrite: true},
		{Name: "edit copy only", Enabled: true, Action: "set", Target: "body", Path: "/metadata/copy/value", ValueJSON: `"changed"`, Overwrite: true},
		{Name: "copy incoming tag", Enabled: true, Action: "copy", Source: "header", From: "x-client-tag", Target: "header", Path: "X-Upstream-Tag", Overwrite: true},
		{Name: "tag to metadata", Enabled: true, Action: "move", Source: "header", From: "X-Upstream-Tag", Target: "body", Path: "/metadata/tags", Overwrite: true},
		{Name: "nested wildcard rename", Enabled: true, Action: "move", Source: "body", From: "/metadata/rows/*/items/*/old", Target: "body", Path: "/metadata/rows/*/items/*/new", Overwrite: true},
		{Name: "set in every message", Enabled: true, Action: "set", Target: "body", Path: "/messages/*/prefix", ValueJSON: "false", Overwrite: true},
		{Name: "create objects below wildcard", Enabled: true, Action: "set", Target: "body", Path: "/metadata/rows/*/added/tag", ValueJSON: `"new"`, Overwrite: true},
		{Name: "delete added prefix", Enabled: true, Action: "delete", Target: "body", Path: "/messages/*/prefix"},
		{Name: "remove array members", Enabled: true, Action: "delete", Target: "body", Path: "/metadata/drop/*"},
		{Name: "literal escaped key", Enabled: true, Action: "set", Target: "body", Path: "/metadata/a~1b~0c", ValueJSON: "null", Overwrite: true},
		{Name: "unused", Enabled: false, Action: "set", Target: "body", Path: "/temperature", ValueJSON: "99", Overwrite: true},
	}}
	incoming := http.Header{"x-client-tag": {"one", "two"}, "Authorization": {"Bearer client-secret"}, "Cookie": {"session=secret"}}
	body, headers, err := editCompletionRequest([]byte(`{"messages":[{"role":"user","content":"hello"}],"temperature":0.1,"metadata":{"source":{"value":"original"},"rows":[{"items":[{"old":1},{"old":2}]},{"items":[{"old":3}]}],"drop":[1,2,3]}}`), incoming, config)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	_ = decodeEditJSON(body, &root)
	metadata := root["metadata"].(map[string]any)
	if root["temperature"].(json.Number) != "0.7" || root["random_seed"].(json.Number) != "9007199254740993" || metadata["source"].(map[string]any)["value"] != "original" || metadata["copy"].(map[string]any)["value"] != "changed" {
		t.Fatal("ordered rules lost numeric precision or copied values by reference")
	}
	if !reflect.DeepEqual(metadata["tags"], []any{"one", "two"}) || len(metadata["drop"].([]any)) != 0 || len(headers) != 0 || len(incoming["x-client-tag"]) != 2 {
		t.Fatal("header rules leaked incoming headers or wildcard deletes shifted indices")
	}
	if !strings.Contains(string(body), `"items":[{"new":1},{"new":2}]`) || !strings.Contains(string(body), `"items":[{"new":3}]`) || strings.Contains(string(body), `"old"`) || strings.Contains(string(body), `"prefix"`) {
		t.Fatalf("nested wildcard paths did not preserve their captures: %s", body)
	}
	if strings.Count(string(body), `"added":{"tag":"new"}`) != 2 {
		t.Fatal("setting a wildcard target did not create missing nested objects")
	}
	if _, exists := metadata["a/b~c"]; !exists || metadata["a/b~c"] != nil {
		t.Fatal("JSON Pointer escapes or JSON null were not preserved")
	}
}

func TestRequestEditMovePreservesSourceWhenTargetIsNotOverwritten(t *testing.T) {
	config := &requestEditConfig{Rules: []requestEditRule{{Name: "move with default", Enabled: true, Action: "move", Source: "body", From: "/metadata/source", Target: "body", Path: "/metadata/target"}}}
	body, _, err := editCompletionRequest([]byte(`{"messages":[],"metadata":{"source":"keep","target":null}}`), nil, config)
	if err != nil || !strings.Contains(string(body), `"source":"keep"`) || !strings.Contains(string(body), `"target":null`) {
		t.Fatalf("skipped move discarded the source: %s, %v", body, err)
	}
}

func TestRequestEditRejectsInvalidRulesAndBadRequestShapes(t *testing.T) {
	isolatedState(t)
	for _, fields := range []string{
		`"action":"set","target":"body","path":"/model","value_json":"1"`,
		`"action":"delete","target":"body","path":"/stream"`,
		`"action":"delete","target":"body","path":"/*"`,
		`"action":"set","target":"header","path":"authorization","value_json":"\"secret\""`,
		`"action":"set","target":"header","path":"X-Test","value_json":"\"bad\\r\\nheader\""`,
		`"action":"set","target":"header","path":"X-Test","value_json":"42"`,
		`"action":"set","target":"body","path":"/x~2","value_json":"0"`,
		`"action":"set","target":"body","path":"temperature","value_json":"0"`,
		`"action":"set","target":"body","path":"/x","value_json":"1 2"`,
		`"action":"copy","source":"body","from":"/messages/*/content","target":"body","path":"/metadata/content"`,
		`"action":"move","source":"body","from":"/metadata","target":"body","path":"/metadata/nested"`,
	} {
		response := managementCall(t, "PATCH", "/request-edit", `{"rules":[{"name":"invalid","enabled":true,`+fields+`}]}`)
		if response.StatusCode != 400 {
			t.Errorf("invalid rule accepted: %s (%s)", fields, response.Body)
		}
	}
	for _, raw := range []string{"null", "[]", `{}`, `{"messages":null}`, `{"messages":[null]}`, `{"messages":{}}`, `{"messages":[]} {}`} {
		if _, _, err := editCompletionRequest([]byte(raw), nil, nil); err == nil {
			t.Errorf("invalid body accepted: %s", raw)
		}
	}
	config := &requestEditConfig{Rules: []requestEditRule{{Name: "wrong parent", Enabled: true, Action: "set", Target: "body", Path: "/metadata/tag", ValueJSON: `"tag"`, Overwrite: true}}}
	if _, _, err := editCompletionRequest([]byte(`{"messages":[],"metadata":"text"}`), nil, config); err == nil {
		t.Fatal("writing through a scalar parent must fail")
	}
}

func TestRequestEditPreviewUsesSavedRulesWithoutNetworkOrSecrets(t *testing.T) {
	isolatedState(t)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		t.Fatal("preview called the host: " + method)
		return nil, nil
	}
	response := managementCall(t, "PATCH", "/request-edit", `{"rules":[{"name":"forward tag","enabled":true,"action":"copy","source":"header","from":"X-Tag","target":"header","path":"X-Tag","overwrite":true},{"name":"exact number","enabled":true,"action":"set","target":"body","path":"/metadata/number","value_json":"9007199254740993","overwrite":true}]}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	stateLoaded = false // Exercise the persisted literal representation, not just memory.
	response = managementCall(t, "POST", "/request-edit/preview", `{"body":{"model":"client","messages":[],"stream":true,"seed":9007199254740993},"headers":{"X-Tag":"preview","Authorization":"Bearer private","Cookie":["secret"]}}`)
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "private") || strings.Contains(string(response.Body), "secret") || strings.Contains(string(response.Body), "9007199254740992") {
		t.Fatalf("preview leaked credentials or lost numeric precision: %s", response.Body)
	}
	var result struct {
		Body    map[string]json.RawMessage `json:"body"`
		Headers http.Header                `json:"headers"`
	}
	if err := json.Unmarshal(response.Body, &result); err != nil || result.Headers.Get("X-Tag") != "preview" || result.Headers.Get("Accept") != "text/event-stream" || string(result.Body["random_seed"]) != "9007199254740993" || !strings.Contains(string(result.Body["metadata"]), "9007199254740993") {
		t.Fatalf("preview did not use saved rules: %s, %v", response.Body, err)
	}
}

func TestRequestEditAppliesToBothExecutorsAndKeepsRoutingAndCredentials(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			isolatedState(t)
			state.Keys = []apiKey{{ID: "key", Enabled: true, Secret: "upstream-secret", Channel: "vibe"}}
			state.Models = []modelMapping{{ID: "model", Enabled: true, ClientModel: "client", UpstreamModel: "upstream", ThinkingMap: "high=medium"}}
			state.RequestEdit = &requestEditConfig{BuiltinEnabled: true, Rules: []requestEditRule{
				{Name: "override native limit", Enabled: true, Action: "set", Target: "body", Path: "/max_tokens", ValueJSON: "24", Overwrite: true},
				{Name: "forward custom header", Enabled: true, Action: "copy", Source: "header", From: "x-client-tag", Target: "header", Path: "X-Upstream-Tag", Overwrite: true},
				{Name: "override reasoning", Enabled: true, Action: "set", Target: "body", Path: "/reasoning_effort", ValueJSON: `"high"`, Overwrite: true},
			}}
			called := false
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				request, ok := payload.(upstreamRequest)
				if !ok {
					t.Fatalf("unexpected callback %s", method)
				}
				called = true
				var body map[string]json.RawMessage
				_ = json.Unmarshal(request.Body, &body)
				if string(body["model"]) != `"upstream"` || string(body["max_tokens"]) != "24" || string(body["reasoning_effort"]) != `"medium"` || body["stream_options"] != nil || body["seed"] != nil || string(body["random_seed"]) != "42" || !strings.Contains(string(body["metadata"]), `"call_source":"vibe_code"`) {
					t.Fatalf("executor did not prepare the edited native request: %s", request.Body)
				}
				if request.Headers.Get("Authorization") != "Bearer upstream-secret" || request.Headers.Get("X-Upstream-Tag") != "test" || request.Headers.Get("X-Unmapped") != "" || request.Headers.Get("Cookie") != "" {
					t.Fatalf("executor forwarded wrong headers: %#v", request.Headers)
				}
				if method == "host.http.do_stream" {
					if string(body["stream"]) != "true" || request.Headers.Get("Accept") != "text/event-stream" {
						t.Fatal("stream mode was not fixed after request editing")
					}
					return json.RawMessage(`{"status_code":503,"body":""}`), nil
				}
				if string(body["stream"]) != "false" {
					t.Fatal("ordinary completion must remain non-streaming")
				}
				return json.RawMessage(`{"StatusCode":503,"Body":""}`), nil
			}
			raw, _ := json.Marshal(rpcExecutorRequest{Model: "client", Payload: []byte(`{"model":"client","messages":[{"role":"developer","content":"test"}],"max_completion_tokens":128,"seed":42,"stream_options":{"include_usage":true}}`), Headers: http.Header{"x-client-tag": {"test"}, "Authorization": {"Bearer client-secret"}, "X-Unmapped": {"ignore"}, "Cookie": {"private"}}})
			_, err := handleMethod(method, raw)
			var status *statusError
			if !called || !errors.As(err, &status) || status.Status != 503 {
				t.Fatalf("edited request did not reach upstream: %v", err)
			}
		})
	}
}
