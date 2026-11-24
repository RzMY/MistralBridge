package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func readTestDebugExchange(t *testing.T, id string) debugExchange {
	t.Helper()
	path, err := debugLogPath(id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var exchange debugExchange
	if err := json.Unmarshal(stored, &exchange); err != nil {
		t.Fatal(err)
	}
	return exchange
}

func TestDebugCapturesTrueOriginalAndExecutorBeforeEditing(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			isolatedState(t)
			state.DebugMode = true
			state.Keys = []apiKey{{ID: "key", Secret: "upstream-secret", Enabled: true}}
			state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "native", Enabled: true}}
			original := []byte("{\n  \"model\":\"alias\",\"messages\":[{\"role\":\"developer\",\"content\":\"test\"}],\"max_completion_tokens\":16,\"seed\":9007199254740993,\"client_option\":{\"keep_original\":true},\"metadata\":{\"echo\":\"client-secret upstream-secret\"}\n}")
			executor := []byte(`{"model":"alias","messages":[{"role":"developer","content":"test"}],"max_completion_tokens":16,"seed":9007199254740993,"client_option":{"keep_executor":true}}`)
			incoming := http.Header{"Authorization": {"Bearer client-secret"}, "X_API_KEY": {"secondary-secret"}, "X-Client-Tag": {"desktop"}}
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				request := payload.(upstreamRequest)
				if strings.Contains(string(request.Body), "client_option") || strings.Contains(string(request.Body), "max_completion_tokens") || !strings.Contains(string(request.Body), `"model":"native"`) || !strings.Contains(string(request.Body), `"random_seed":9007199254740993`) {
					t.Fatalf("upstream request was not edited: %s", request.Body)
				}
				if method == "host.http.do_stream" {
					return json.Marshal(upstreamStreamResponse{StatusCode: 503})
				}
				return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[]}`)})
			}
			raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Format: "openai", SourceFormat: "openai", OriginalRequest: original, Payload: executor, Headers: incoming})
			_, err := handleMethod(method, raw)
			if method == "executor.execute" && err != nil {
				t.Fatal(err)
			}
			entry := state.Logs[0]
			if !entry.DebugAvailable || !entry.OriginalRequestAvailable || !entry.ExecutorRequestAvailable {
				t.Fatalf("debug request sources missing from log: %#v", entry)
			}
			exchange := readTestDebugExchange(t, entry.ID)
			expected := strings.ReplaceAll(strings.ReplaceAll(string(original), "client-secret", debugRedactedText), "upstream-secret", debugRedactedText)
			if exchange.OriginalRequest == nil || exchange.OriginalRequest.Body != expected || exchange.OriginalRequest.Format != "openai" || exchange.ExecutorRequest == nil || exchange.ExecutorRequest.Body != string(executor) {
				t.Fatalf("original/executor snapshots changed: %#v", exchange)
			}
			if exchange.OriginalRequest.Headers.Get("Authorization") != debugRedactedText || exchange.OriginalRequest.Headers["X_API_KEY"][0] != debugRedactedText || exchange.ExecutorRequest.Headers.Get("X-Client-Tag") != "desktop" {
				t.Fatal("snapshot authentication was not redacted or custom headers were lost")
			}
			if !strings.Contains(exchange.Request.Body, `"role":"system"`) || strings.Contains(exchange.Request.Body, "client_option") || !strings.Contains(exchange.OriginalRequest.Body, `"role":"developer"`) {
				t.Fatal("original and actual upstream requests were not kept separate")
			}
			for kind, expected := range map[string]string{"original_request": exchange.OriginalRequest.Body, "executor_request": exchange.ExecutorRequest.Body, "upstream_request": exchange.Request.Body} {
				query := url.Values{"id": {entry.ID}}
				if kind != "original_request" {
					query.Set("kind", kind)
				}
				response := previewDebugAPI(t, "debug-record", query)
				var imported struct {
					Body string `json:"body_json"`
					Kind string `json:"kind"`
				}
				_ = json.Unmarshal(response.Body, &imported)
				if response.StatusCode != 200 || imported.Body != expected || imported.Kind != kind {
					t.Fatalf("%s import mismatch: %s", kind, response.Body)
				}
			}
			path, _ := statePath()
			persisted := readRequestLogs(path)
			if !persisted[0].OriginalRequestAvailable || !persisted[0].ExecutorRequestAvailable {
				t.Fatal("snapshot availability flags did not persist")
			}
		})
	}
}

func TestDebugOriginalCaptureBeforeValidationAndOnlyWhenEnabled(t *testing.T) {
	isolatedState(t)
	state.DebugMode = true
	incoming := http.Header{"Authorization": {"Bearer client-key"}, "X-Client": {"original"}}
	request := rpcExecutorRequest{Model: "missing-model", SourceFormat: "openai", OriginalRequest: []byte(`{"messages":[],"client_option":true}`), Payload: []byte(`{"messages":[]}`), Headers: incoming}
	trace := newRequestTrace(request)
	request.OriginalRequest[0] = 'x'
	request.Payload[0] = 'x'
	incoming.Set("X-Client", "mutated")
	trace.finish(nil, &statusError{Status: 404, Message: "mapping missing"})
	exchange := readTestDebugExchange(t, state.Logs[0].ID)
	if exchange.OriginalRequest == nil || exchange.OriginalRequest.Body[0] != '{' || exchange.ExecutorRequest.Body[0] != '{' || exchange.OriginalRequest.Headers.Get("X-Client") != "original" || exchange.Request.Method != "" {
		t.Fatal("capture was not isolated before request validation")
	}
	state.DebugMode = false
	trace = newRequestTrace(request)
	if trace.debug != nil {
		t.Fatal("disabled debug retained original request data")
	}
	trace.finish(nil, nil)
	if state.Logs[1].OriginalRequestAvailable || state.Logs[1].DebugAvailable {
		t.Fatal("disabled debug incorrectly advertised a saved original request")
	}
}

func TestDebugImportLegacyNeverSubstitutesUpstreamForOriginal(t *testing.T) {
	isolatedState(t)
	for i, executorOnly := range []bool{false, true} {
		id := fmt.Sprintf("%024x", i+1)
		exchange := &debugExchange{ID: id, Request: debugMessage{Method: "POST", Body: `{"messages":[],"model":"native"}`}}
		if executorOnly {
			exchange.ExecutorRequest = &debugMessage{Format: "openai", Body: `{"messages":[],"model":"client"}`}
		}
		if err := saveDebugLog(exchange, ""); err != nil {
			t.Fatal(err)
		}
		response := previewDebugAPI(t, "debug-record", url.Values{"id": {id}})
		if response.StatusCode != 409 || !strings.Contains(string(response.Body), "旧版记录无法恢复原始请求") {
			t.Fatalf("legacy upstream was mislabeled as original: %s", response.Body)
		}
		response = previewDebugAPI(t, "debug-record", url.Values{"id": {id}, "kind": {"upstream_request"}})
		if response.StatusCode != 200 {
			t.Fatal("explicit legacy upstream import no longer works")
		}
		response = previewDebugAPI(t, "debug-record", url.Values{"id": {id}, "kind": {"not-a-source"}})
		if response.StatusCode != 400 {
			t.Fatal("invalid request source was accepted")
		}
	}
}

func TestOriginalResponsesRequestIsNotConfusedWithTranslatedChat(t *testing.T) {
	isolatedState(t)
	state.DebugMode = true
	raw := `{"model":"client","input":[{"role":"developer","content":"original"}],"max_output_tokens":16,"client_option":42}`
	payload := `{"model":"client","messages":[{"role":"developer","content":"translated"}],"max_tokens":16}`
	trace := newRequestTrace(rpcExecutorRequest{Model: "client", SourceFormat: "openai", Format: "openai", OriginalRequest: []byte(raw), Payload: []byte(payload)})
	trace.finish(nil, &statusError{Status: 400, Message: "test"})
	response := previewDebugAPI(t, "debug-record", url.Values{"id": {trace.entry.ID}})
	var imported struct {
		Body   string `json:"body_json"`
		Format string `json:"format"`
	}
	_ = json.Unmarshal(response.Body, &imported)
	if response.StatusCode != 200 || imported.Body != raw || imported.Format != "openai-response" {
		t.Fatalf("true Responses body was not preserved: %s", response.Body)
	}
	preview := managementCall(t, "POST", "/request-edit/preview", `{"body":`+raw+`,"headers":{},"source_format":"openai-response"}`)
	if preview.StatusCode != 400 || !strings.Contains(string(preview.Body), "导入执行器输入") {
		t.Fatal("non-Chat original was silently previewed as a different protocol")
	}
	response = previewDebugAPI(t, "debug-record", url.Values{"id": {trace.entry.ID}, "kind": {"executor_request"}})
	_ = json.Unmarshal(response.Body, &imported)
	if response.StatusCode != 200 || imported.Body != payload || imported.Format != "openai" {
		t.Fatal("explicit executor input import failed")
	}
	if _, err := readPreviewDebugRequest(strings.NewReader(`{"request":{"method":"POST"},"response":not-json`), "original_request"); !errors.Is(err, errDebugRequestMissing) {
		t.Fatal("legacy original lookup attempted to read the response")
	}
}
