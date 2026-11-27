package bridge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func previewDebugAPI(t *testing.T, path string, query url.Values) managementResponse {
	t.Helper()
	raw, _ := json.Marshal(managementRequest{Method: "GET", Path: "/plugins/mistral-bridge/request-edit/" + path, Query: query})
	result, err := HandleMethod("management.handle", raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(result, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope: %s, %v", result, err)
	}
	var response managementResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestPreviewDebugPickerPaginationSearchAndExpiredRecords(t *testing.T) {
	isolatedState(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%024x", i+1)
		created := time.Date(2026, 11, 24, 8, i, 0, 0, time.UTC)
		exchange := &debugExchange{ID: id, CreatedAt: created, Request: debugMessage{Method: "POST", Body: `{"messages":[]}`}}
		if err := saveDebugLog(exchange, ""); err != nil {
			t.Fatal(err)
		}
		state.Logs = append(state.Logs, RequestLog{ID: id, Model: "client", UpstreamModel: "native", KeyLabel: "Production", Status: "ok", HTTPStatus: 200, CreatedAt: created, DebugAvailable: true})
	}
	expired := state.Logs[0]
	expired.ID = strings.Repeat("f", 24)
	state.Logs = append(state.Logs, expired, RequestLog{ID: strings.Repeat("e", 24), Model: "without-debug"})
	response := previewDebugAPI(t, "debug-records", nil)
	var data struct {
		Records []previewDebugRecord `json:"records"`
		Total   int                  `json:"total"`
		Page    int                  `json:"page"`
		Pages   int                  `json:"pages"`
	}
	if err := json.Unmarshal(response.Body, &data); err != nil || response.StatusCode != 200 {
		t.Fatalf("picker response: %s, %v", response.Body, err)
	}
	if data.Total != 12 || data.Pages != 2 || data.Page != 1 || len(data.Records) != 10 || data.Records[0].ID != fmt.Sprintf("%024x", 12) {
		t.Fatalf("picker did not filter unavailable records or sort newest first: %s", response.Body)
	}
	response = previewDebugAPI(t, "debug-records", url.Values{"page": {"2"}, "search": {"production"}})
	_ = json.Unmarshal(response.Body, &data)
	if len(data.Records) != 2 || data.Page != 2 || data.Total != 12 {
		t.Fatal("picker search/pagination mismatch: " + string(response.Body))
	}
	response = previewDebugAPI(t, "debug-records", url.Values{"search": {"missing"}})
	if !strings.Contains(string(response.Body), `"records":[]`) || !strings.Contains(string(response.Body), `"total":0`) {
		t.Fatal("empty picker response must be an empty array")
	}
}

func TestPreviewDebugImportPreservesSavedRequestAndNeverCallsUpstream(t *testing.T) {
	isolatedState(t)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		t.Fatal("debug import called host: " + method)
		return nil, nil
	}
	id := strings.Repeat("1", 24)
	body := `{"messages":[{"role":"user","content":"text"}],"random_seed":9007199254740993,"metadata":{"literal":"upstream-secret"}}`
	exchange := &debugExchange{ID: id, CreatedAt: time.Now().UTC(), Request: debugMessage{Method: "POST", Headers: map[string][]string{"Authorization": {"Bearer upstream-secret"}, "X-Request-Tag": {"one", "two"}}, Body: body}}
	if err := saveDebugLog(exchange, "upstream-secret"); err != nil {
		t.Fatal(err)
	}
	state.Logs = append(state.Logs, RequestLog{ID: id, Model: "client", UpstreamModel: "native", DebugAvailable: true, CreatedAt: exchange.CreatedAt})
	response := previewDebugAPI(t, "debug-record", url.Values{"id": {id}, "kind": {"upstream_request"}})
	var imported struct {
		BodyJSON string              `json:"body_json"`
		Headers  map[string][]string `json:"headers"`
		Source   previewDebugRecord  `json:"source"`
		Kind     string              `json:"kind"`
	}
	if err := json.Unmarshal(response.Body, &imported); err != nil || response.StatusCode != 200 {
		t.Fatalf("import response: %s, %v", response.Body, err)
	}
	if imported.BodyJSON != strings.ReplaceAll(body, "upstream-secret", debugRedactedText) || imported.Headers["Authorization"][0] != debugRedactedText || len(imported.Headers["X-Request-Tag"]) != 2 || imported.Source.Model != "client" || imported.Kind != "upstream_request" {
		t.Fatalf("saved request was not imported exactly: %s", response.Body)
	}
	if strings.Contains(string(response.Body), "upstream-secret") || strings.Contains(string(response.Body), "9007199254740992") {
		t.Fatal("import exposed credentials or rounded an integer")
	}
	path, _ := statePath()
	if _, err := os.Stat(path); !os.IsNotExist(err) || len(state.Logs) != 1 {
		t.Fatal("import changed plugin state or created a request log")
	}
}

func TestPreviewDebugImportRejectsMissingInvalidAndOversizedRequests(t *testing.T) {
	isolatedState(t)
	for _, query := range []url.Values{{"id": {"../../state"}}, {"id": {""}}} {
		if response := previewDebugAPI(t, "debug-record", query); response.StatusCode != 400 {
			t.Fatal("invalid import id accepted")
		}
	}
	if response := previewDebugAPI(t, "debug-record", url.Values{"id": {strings.Repeat("d", 24)}}); response.StatusCode != 404 {
		t.Fatal("expired import did not return 404")
	}
	for i, body := range []string{"", "null", "[]", "not JSON", `{"messages":[`, `{"messages":[],"content":"` + strings.Repeat("x", maxPreviewRequestBytes) + `"}`} {
		id := fmt.Sprintf("%024x", i+1)
		exchange := &debugExchange{ID: id, Request: debugMessage{Method: "POST", Body: body}}
		if err := saveDebugLog(exchange, ""); err != nil {
			t.Fatal(err)
		}
		want := 422
		if i == 5 {
			want = 413
		}
		if response := previewDebugAPI(t, "debug-record", url.Values{"id": {id}, "kind": {"upstream_request"}}); response.StatusCode != want {
			t.Fatalf("body %d: got %d, want %d", i, response.StatusCode, want)
		}
	}
}

func TestPreviewDebugReaderStopsBeforeLargeResponse(t *testing.T) {
	raw := `{"id":"id","created_at":"2026-11-24T08:00:00Z","duration_ms":1,"request":{"method":"POST","body":"{\"messages\":[]}"},"response":this response is intentionally not JSON`
	request, err := readPreviewDebugRequest(strings.NewReader(raw), "request")
	if err != nil || request.Body != `{"messages":[]}` {
		t.Fatalf("reader consumed response instead of stopping after request: %#v, %v", request, err)
	}
}
