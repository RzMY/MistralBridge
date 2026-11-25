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
	"unicode/utf8"
)

func TestAutoDropSchemaProjectionAndOpenPayloads(t *testing.T) {
	input := `{
		"model":"client","messages":[
			{"role":"system","content":[{"type":"text","text":"instructions","extra":"system chunk"}],"extra":"system"},
			{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png","detail":"high","extra":42},"extra":"image chunk"},{"type":"resource","uri":"app://document","content":{"type":"text","text":"nested content","extra":true}}],"extra":"user"},
			{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"thought","extra":"thought chunk"}],"signature":"signed","closed":true,"extra":"think chunk"}],"tool_calls":[{"id":"call","type":"function","index":0,"function":{"name":"fn","arguments":{"client_extension":true,"store":false},"extra":"function call"},"extra":"tool call"}],"extra":"assistant"},
			{"role":"tool","content":"result","name":"fn","tool_call_id":"call","extra":"tool"}
		],
		"tools":[{"type":"function","function":{"name":"fn","strict":true,"parameters":{"type":"object","properties":{"client_extension":{"type":"boolean","extra":"schema keyword"}}},"extra":"definition"},"extra":"tool"},{"type":"connector","connector_id":"conn","authorization":{"type":"api-key","value":"connector-key","extra":"auth option"},"tool_configuration":{"include":["one"],"extra":"config option"},"extra":"connector option"}],
		"tool_choice":{"type":"function","function":{"name":"fn","extra":"function name"},"extra":"choice"},
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"extra":{"type":"string"}},"custom_json_schema_keyword":true},"strict":true,"extra":"json schema option"},"extra":"format option"},
		"metadata":{"arbitrary":{"store":false,"client_extension":true}},"top_logprobs":3,"prompt_cache_key":"cache","service_tier":"auto","safe_prompt":false,"prediction":{"type":"content","content":"expected","extra":"prediction option"},
		"client_extension":{"nested":true}
	}`
	report := newRequestDropReport(nil, "")
	body, _, err := editCompletionRequestWithReport([]byte(input), nil, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, field := range report.Fields {
		paths[field.Path] = field.Value
	}
	for _, path := range []string{
		"/client_extension", "/messages/0/extra", "/messages/0/content/0/extra", "/messages/1/content/0/image_url/extra", "/messages/1/content/1/content/extra", "/messages/2/content/0/thinking/0/extra", "/messages/2/content/0/extra", "/messages/2/tool_calls/0/function/extra", "/tools/0/function/extra", "/tools/1/authorization/extra", "/tools/1/tool_configuration/extra", "/tool_choice/function/extra", "/response_format/json_schema/extra", "/prediction/extra",
	} {
		if _, exists := paths[path]; !exists {
			t.Errorf("missing discarded field %s; paths=%v", path, paths)
		}
	}
	for _, kept := range []string{`"client_extension":true`, `"extra":"schema keyword"`, `"custom_json_schema_keyword":true`, `"arguments":{"client_extension":true,"store":false}`, `"signature":"signed"`, `"closed":true`, `"top_logprobs":3`, `"prompt_cache_key":"cache"`, `"service_tier":"auto"`, `"safe_prompt":false`} {
		if !strings.Contains(string(body), kept) {
			t.Errorf("valid native or arbitrary payload was modified: %s\n%s", kept, body)
		}
	}
	if paths["/messages/1/content/0/image_url/extra"] != "42" || report.Count != len(report.Fields) {
		t.Fatal("drop details lost values or failed to count all fields")
	}
}

func TestAutoDropDoesNotGuessInvalidValuesOrUnknownVariants(t *testing.T) {
	input := []byte(`{"messages":[{"role":"future-role","content":"keep","extra":"keep"},{"role":"user","content":[{"type":"future-type","payload":{"extra":"keep"}}]}],"temperature":"invalid but present","tools":[{"type":"future-tool","extra":"keep"}],"metadata":{"anything":true},"client/option~x":9007199254740993}`)
	report := newRequestDropReport(nil, "")
	body, _, err := editCompletionRequestWithReport(input, nil, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	if report.Count != 1 || report.Fields[0].Path != "/client~1option~0x" || report.Fields[0].Value != "9007199254740993" {
		t.Fatalf("wrong drop report: %#v", report)
	}
	if !strings.Contains(string(body), `"temperature":"invalid but present"`) || !strings.Contains(string(body), `"future-type"`) || !strings.Contains(string(body), `"future-tool"`) || strings.Count(string(body), `"extra":"keep"`) != 3 {
		t.Fatal("filter guessed unknown variants or rewrote a known invalid value")
	}
}

func TestAutoDropRunsAfterRulesAndCanBeDisabledIndependently(t *testing.T) {
	config := &requestEditConfig{BuiltinEnabled: true, Rules: []requestEditRule{
		{Name: "recover token limit", Enabled: true, Action: "move", Source: "body", From: "/client_limit", Target: "body", Path: "/max_tokens", Overwrite: true},
		{Name: "preserve tag", Enabled: true, Action: "copy", Source: "body", From: "/client_tag", Target: "body", Path: "/metadata/client_tag", Overwrite: true},
		{Name: "add extension", Enabled: true, Action: "set", Target: "body", Path: "/custom_added", ValueJSON: "true", Overwrite: true},
		{Name: "explicit delete", Enabled: true, Action: "delete", Target: "body", Path: "/user_deleted"},
		{Name: "custom header", Enabled: true, Action: "set", Target: "header", Path: "X-Custom", ValueJSON: `"keep"`, Overwrite: true},
	}}
	input := []byte(`{"messages":[],"seed":42,"client_limit":24,"client_tag":"desktop","user_deleted":true}`)
	report := newRequestDropReport(nil, "")
	body, headers, err := editCompletionRequestWithReport(input, nil, config, report)
	if err != nil || report.Count != 2 || !strings.Contains(string(body), `"max_tokens":24`) || !strings.Contains(string(body), `"metadata":{"client_tag":"desktop"}`) || headers.Get("X-Custom") != "keep" {
		t.Fatalf("fallback did not run after custom rules: %s, %#v, %v", body, report, err)
	}
	for _, field := range report.Fields {
		if field.Path != "/client_tag" && field.Path != "/custom_added" {
			t.Fatalf("mapped or explicitly deleted field was logged as an automatic drop: %#v", field)
		}
	}
	disabled := false
	config.AutoDropIncompatible = &disabled
	report = newRequestDropReport(nil, "")
	body, _, err = editCompletionRequestWithReport(input, nil, config, report)
	if err != nil || report.Count != 0 || !strings.Contains(string(body), `"custom_added":true`) || !strings.Contains(string(body), `"random_seed":42`) {
		t.Fatal("disabling the fallback changed builtins or custom rules")
	}
	config.BuiltinEnabled = false
	config.AutoDropIncompatible = nil
	report = newRequestDropReport(nil, "")
	body, _, err = editCompletionRequestWithReport(input, nil, config, report)
	if err != nil || strings.Contains(string(body), `"seed":`) || strings.Contains(string(body), `"random_seed":`) || report.Count != 3 {
		t.Fatal("builtin and auto-drop switches were coupled")
	}
}

func TestAutoDropSettingsMigrationPersistenceAndRollback(t *testing.T) {
	isolatedState(t)
	path, _ := statePath()
	if err := os.WriteFile(path, []byte(`{"keys":[],"models":[],"catalog":[],"request_edit":{"builtin_enabled":false,"rules":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	stateLoaded = false
	if config := currentState().RequestEdit; !autoDropEnabled(config) || config.BuiltinEnabled {
		t.Fatal("old persisted request edit settings did not default auto-drop to enabled")
	}
	response := managementCall(t, "PATCH", "/request-edit", `{"auto_drop_incompatible":false}`)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"auto_drop_incompatible":false`) {
		t.Fatalf("new switch save failed: %s", response.Body)
	}
	stateLoaded = false
	config := currentState().RequestEdit
	if autoDropEnabled(config) || config.BuiltinEnabled {
		t.Fatal("saved disabled fallback did not survive reload")
	}
	*config.AutoDropIncompatible = true
	if autoDropEnabled(currentState().RequestEdit) {
		t.Fatal("switch snapshots alias mutable persisted settings")
	}
	response = managementCall(t, "PATCH", "/request-edit", `{"builtin_enabled":true,"rules":[]}`)
	if response.StatusCode != 200 || autoDropEnabled(currentState().RequestEdit) {
		t.Fatal("editing other settings reset the fallback toggle")
	}
	response = managementCall(t, "PATCH", "/request-edit", `{"auto_drop_incompatible":"true"}`)
	if response.StatusCode != 400 || autoDropEnabled(currentState().RequestEdit) {
		t.Fatal("invalid toggle was accepted")
	}
	t.Setenv("MISTRAL_BRIDGE_DATA_DIR", path+"/not-a-directory")
	response = managementCall(t, "PATCH", "/request-edit", `{"auto_drop_incompatible":true}`)
	if response.StatusCode != 500 || autoDropEnabled(currentState().RequestEdit) {
		t.Fatal("failed toggle save was not rolled back")
	}
}

func TestAutoDropDetailsAreBoundedRedactedAndUTF8(t *testing.T) {
	incoming := http.Header{"Authorization": {"Bearer client-key"}, "X-Api-Key": {"header-secret"}}
	report := newRequestDropReport(incoming, "mistral-key")
	input := []byte(`{"messages":[],"client_extension":{"value":"mistral-key and client-key and header-secret","password":"private","nested":{"api_key":"private"},"allowed":42},"custom_secret":"private"}`)
	_, _, err := editCompletionRequestWithReport(input, incoming, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"mistral-key", "client-key", "header-secret", "private"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("dropped fields exposed %s", secret)
		}
	}
	if !strings.Contains(string(encoded), "redacted") || !strings.Contains(string(encoded), "allowed") {
		t.Fatal("drop report lost its useful non-sensitive contents")
	}
	report = newRequestDropReport(nil, "")
	for i := 0; i < 200; i++ {
		report.add(fmt.Sprintf("/field%d", i), "field", strings.Repeat("中文", 3000))
	}
	encoded, _ = json.Marshal(report)
	if report.Count != 200 || len(report.Fields) >= 200 || len(encoded) > 22000 {
		t.Fatalf("unbounded drop logging: count=%d stored=%d bytes=%d", report.Count, len(report.Fields), len(encoded))
	}
	for _, field := range report.Fields {
		if !field.Truncated || !utf8.ValidString(field.Value) {
			t.Fatal("truncated Unicode detail is invalid")
		}
	}
}

func TestAutoDropPreviewAndBothExecutorLogsWithoutDebug(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			isolatedState(t)
			state.Keys = []apiKey{{ID: "key", Enabled: true, Secret: "mistral-key"}}
			state.Models = []modelMapping{{ID: "model", Enabled: true, ClientModel: "client", UpstreamModel: "upstream"}}
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				request, ok := payload.(upstreamRequest)
				if !ok || strings.Contains(string(request.Body), "client_extension") || strings.Contains(string(request.Body), "message_extension") {
					t.Fatal("incompatible fields reached upstream")
				}
				if method == "host.http.do_stream" {
					return json.RawMessage(`{"status_code":503,"body":""}`), nil
				}
				return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)})
			}
			raw, _ := json.Marshal(rpcExecutorRequest{Model: "client", Payload: []byte(`{"messages":[{"role":"user","content":"test","message_extension":false}],"client_extension":"desktop"}`)})
			_, err := handleMethod(method, testNativeRaw(raw))
			if method == "executor.execute" && err != nil {
				t.Fatal(err)
			}
			if method == "executor.execute_stream" {
				var status *statusError
				if !errors.As(err, &status) || status.Status != 503 {
					t.Fatalf("unexpected stream result: %v", err)
				}
			}
			row := state.Logs[len(state.Logs)-1]
			if row.DroppedCount != 2 || len(row.DroppedFields) != 2 || row.DebugAvailable {
				t.Fatalf("drop report depends on debug or is missing: %#v", row)
			}
			path, _ := statePath()
			stored := readRequestLogs(path)
			if len(stored) != 1 || stored[0].DroppedCount != 2 || stored[0].DroppedFields[0].Value != `"desktop"` {
				t.Fatal("drop report was not persisted in request logs")
			}
			for _, term := range []string{"client_extension", "desktop"} {
				_, err := queryRequestLogs(url.Values{"search": {term}})
				if err != nil {
					t.Fatal(err)
				}
				filtered := filterRequestLogs(stored, logFilter{Search: term, Page: 1, PageSize: 50})
				if filtered["total"].(int) != 1 {
					t.Fatal("discarded paths/values are not searchable")
				}
			}
			preview := managementCall(t, "POST", "/request-edit/preview", `{"body":{"messages":[],"client_extension":"preview"},"headers":{}}`)
			if preview.StatusCode != 200 || !strings.Contains(string(preview.Body), `"dropped_count": 1`) || !strings.Contains(string(preview.Body), `"path": "/client_extension"`) {
				t.Fatalf("preview omitted drop details: %s", preview.Body)
			}
		})
	}
}
