package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDebugModeCapturesFullCompletionAndRedactsCredential(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "key", Secret: "upstream-secret", Label: "primary", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
	response := managementCall(t, "PATCH", "/debug", `{"enabled":true}`)
	if response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	stateLoaded = false
	loadState()
	if !currentState().DebugMode {
		t.Fatal("debug mode was not persisted")
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method != "host.http.do" {
			t.Fatalf("unexpected host call %q", method)
		}
		request := payload.(upstreamRequest)
		if request.Headers.Get("Authorization") != "Bearer upstream-secret" || !strings.Contains(string(request.Body), `"model":"upstream"`) {
			t.Fatalf("request was not transformed as expected: %+v", request)
		}
		body := `{"choices":[{"message":{"content":"ok"}}],"debug":"upstream-secret"}`
		return json.Marshal(upstreamResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)})
	}
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[{"role":"user","content":"question"}]}`)})
	if _, err := execute(raw); err != nil {
		t.Fatal(err)
	}
	entry := state.Logs[0]
	if !entry.DebugAvailable {
		t.Fatal("debug record was not attached to the request log")
	}
	path, err := debugLogPath(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "upstream-secret") {
		t.Fatal("debug data persisted the API credential")
	}
	result, err := queryDebugLog(url.Values{"id": {entry.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var envelopeValue envelope
	if json.Unmarshal(result, &envelopeValue) != nil || !envelopeValue.OK {
		t.Fatalf("debug API response = %s", result)
	}
	var management managementResponse
	if json.Unmarshal(envelopeValue.Result, &management) != nil || management.StatusCode != http.StatusOK {
		t.Fatalf("debug API result = %s", envelopeValue.Result)
	}
	var exchange debugExchange
	if err := json.Unmarshal(management.Body, &exchange); err != nil {
		t.Fatal(err)
	}
	if exchange.Request.Method != http.MethodPost || !strings.Contains(exchange.Request.Body, `"model":"upstream"`) {
		t.Fatalf("request capture = %+v", exchange.Request)
	}
	if exchange.Request.Headers.Get("Authorization") != debugRedactedText {
		t.Fatalf("authorization header was not redacted: %v", exchange.Request.Headers)
	}
	if exchange.Response.StatusCode != http.StatusOK || !exchange.Response.Complete || !strings.Contains(exchange.Response.Body, debugRedactedText) {
		t.Fatalf("response capture = %+v", exchange.Response)
	}

	response = managementCall(t, "PATCH", "/debug", `{"enabled":false}`)
	if response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	if _, err := execute(raw); err != nil {
		t.Fatal(err)
	}
	if !state.Logs[0].DebugAvailable || state.Logs[1].DebugAvailable {
		t.Fatal("debug mode did not apply only to requests started while enabled")
	}
	response = managementCall(t, "DELETE", "/debug", "")
	if response.StatusCode != http.StatusOK || debugLogExists(entry.ID) {
		t.Fatal("debug data was not cleared")
	}
}

func TestDebugModeCapturesStreamingErrorResponse(t *testing.T) {
	isolatedState(t)
	state.DebugMode = true
	state.Keys = []apiKey{{ID: "key", Secret: "test-secret", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return json.Marshal(upstreamStreamResponse{StatusCode: http.StatusBadRequest, Headers: http.Header{"Content-Type": []string{"application/json"}}, StreamID: "upstream-stream"})
		case "host.http.stream_read":
			return json.Marshal(streamReadResponse{Payload: []byte(`{"error":{"message":"unsupported reasoning_effort"}}`), Done: true})
		case "host.http.stream_close":
			return json.Marshal(map[string]bool{"ok": true})
		default:
			return nil, errors.New("unexpected host call: " + method)
		}
	}
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Stream: true, HostCallbackID: "callback", Payload: []byte(`{"messages":[{"role":"user","content":"question"}],"reasoning_effort":"high"}`)})
	if _, err := executeStream(raw); err == nil || !strings.Contains(err.Error(), "unsupported reasoning_effort") {
		t.Fatalf("stream error = %v", err)
	}
	entry := state.Logs[0]
	if entry.HTTPStatus != http.StatusBadRequest || entry.Error != "unsupported reasoning_effort" || !entry.DebugAvailable {
		t.Fatalf("request log = %+v", entry)
	}
	result, err := queryDebugLog(url.Values{"id": {entry.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var envelopeValue envelope
	if json.Unmarshal(result, &envelopeValue) != nil || !envelopeValue.OK {
		t.Fatalf("debug API response = %s", result)
	}
	var management managementResponse
	if json.Unmarshal(envelopeValue.Result, &management) != nil {
		t.Fatal("invalid management response")
	}
	var exchange debugExchange
	if err := json.Unmarshal(management.Body, &exchange); err != nil {
		t.Fatal(err)
	}
	if exchange.Response.StatusCode != http.StatusBadRequest || !exchange.Response.Complete || exchange.Response.Body != `{"error":{"message":"unsupported reasoning_effort"}}` {
		t.Fatalf("stream response capture = %+v", exchange.Response)
	}
}

func TestDebugModeCapturesFullStreamingResponse(t *testing.T) {
	isolatedState(t)
	state.DebugMode = true
	state.Keys = []apiKey{{ID: "key", Secret: "test-secret", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
	responseBody := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return json.Marshal(upstreamStreamResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, StreamID: "upstream-stream"})
		case "host.http.stream_read":
			return json.Marshal(streamReadResponse{Payload: responseBody, Done: true})
		case "host.stream.emit", "host.stream.close", "host.http.stream_close":
			return json.Marshal(map[string]bool{"ok": true})
		default:
			return nil, errors.New("unexpected host call: " + method)
		}
	}
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Stream: true, HostCallbackID: "callback", StreamID: "client-stream", Payload: []byte(`{"messages":[{"role":"user","content":"question"}]}`)})
	if _, err := executeStream(raw); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var entry RequestLog
	for time.Now().Before(deadline) {
		stateMu.RLock()
		if len(state.Logs) > 0 {
			entry = state.Logs[0]
		}
		stateMu.RUnlock()
		if entry.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if entry.ID == "" || !entry.DebugAvailable || entry.Status != "ok" {
		t.Fatalf("request log = %+v", entry)
	}
	path, err := debugLogPath(entry.ID)
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
	if !exchange.Response.Complete || exchange.Response.Body != string(responseBody) {
		t.Fatalf("stream response capture = %+v", exchange.Response)
	}
}
