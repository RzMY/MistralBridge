package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChannelAppliesToBothCompletionPaths(t *testing.T) {
	for _, channel := range []string{"vibe", "studio"} {
		for _, stream := range []bool{false, true} {
			t.Run(channel+map[bool]string{false: "/chat", true: "/stream"}[stream], func(t *testing.T) {
				isolatedState(t)
				state.Keys = []apiKey{{ID: "key", Secret: "secret", Source: "api", Channel: channel, Enabled: true}}
				state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
				called := false
				hostCall = func(method string, payload any) (json.RawMessage, error) {
					called = true
					req := payload.(upstreamRequest)
					wantMethod := "host.http.do"
					if stream {
						wantMethod = "host.http.do_stream"
					}
					var body struct {
						Model    string            `json:"model"`
						Stream   bool              `json:"stream"`
						Metadata map[string]string `json:"metadata"`
					}
					if json.Unmarshal(req.Body, &body) != nil || method != wantMethod || body.Model != "upstream" || body.Stream != stream || body.Metadata["user_id"] != "preserved" {
						t.Fatalf("upstream %s: %s", method, req.Body)
					}
					value, present := body.Metadata["call_source"]
					if (channel == "vibe" && value != "vibe_code") || (channel == "studio" && present) {
						t.Fatalf("wrong pool marker: %s", req.Body)
					}
					if stream {
						return json.Marshal(upstreamStreamResponse{StatusCode: 400, Body: []byte(`{"message":"test response"}`)})
					}
					return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)})
				}
				req := rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[],"metadata":{"call_source":"client_override","user_id":"preserved"}}`)}
				trace := newRequestTrace(req)
				if stream {
					_, _ = performCompletionStream(context.Background(), testNativeRequest(req), trace)
				} else if _, _, err := performCompletion(context.Background(), testNativeRequest(req), trace); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatal("no upstream request")
				}
			})
		}
	}
}

func TestChannelMetadataDefaultsAndValidation(t *testing.T) {
	for _, raw := range []string{`{"messages":[]}`, `{"messages":[],"metadata":null}`, `{"messages":[],"metadata":{"call_source":"vibe_code"}}`} {
		body, err := applyKeyChannel([]byte(raw), apiKey{Channel: "studio"})
		if err != nil || strings.Contains(string(body), `"metadata"`) {
			t.Fatalf("Studio retained empty or Vibe metadata: %s, %v", body, err)
		}
	}
	for _, raw := range []string{`{"metadata":[]}`, `{"metadata":"bad"}`} {
		if _, err := applyKeyChannel([]byte(raw), apiKey{Channel: "vibe"}); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	key := apiKey{Quota: &keyQuota{Kind: "chat"}}
	if activeChannel(key) != "vibe" || publicKey(key).Channel != "auto" {
		t.Fatal("legacy source was not preserved")
	}
	raw := []byte(`{"messages":[],"metadata":{"call_source":"client"}}`)
	got, err := applyKeyChannel(raw, apiKey{})
	if err != nil || string(got) != string(raw) {
		t.Fatal("unknown auto source changed the existing request")
	}
}

func TestChannelSwitchPersistsSourceAndBudgets(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "key", Label: "original", Secret: "secret", BaseURL: defaultBaseURL, Enabled: true, Priority: 7, Quota: &keyQuota{Kind: "chat", Available: true, Limit: 255}}}
	response := managementCall(t, "PATCH", "/keys/channel", `{"id":"key","channel":"studio"}`)
	var key safeKey
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &key) != nil || key.Source != "chat" || key.ActiveChannel != "studio" || key.Quota == nil || key.Quota.Available || key.Quotas.Vibe.Limit != 255 {
		t.Fatalf("switch result: %s", response.Body)
	}
	response = managementCall(t, "PATCH", "/keys", `{"id":"key","label":"edited","enabled":true,"priority":7}`)
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &key) != nil || key.Channel != "studio" || key.Source != "chat" {
		t.Fatalf("older client's edit reset the channel: %s", response.Body)
	}
	stateLoaded = false
	loadState()
	if state.Keys[0].Channel != "studio" || state.Keys[0].Source != "chat" || state.Keys[0].Secret != "secret" || state.Keys[0].Priority != 7 {
		t.Fatal("restart lost credential settings")
	}
	response = managementCall(t, "PATCH", "/keys/channel", `{"id":"key","channel":"auto"}`)
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &key) != nil || key.ActiveChannel != "vibe" {
		t.Fatalf("auto did not restore the original source: %s", response.Body)
	}
	for _, endpoint := range []string{"/keys/channel", "/keys"} {
		if managementCall(t, "PATCH", endpoint, `{"id":"key","channel":"invalid"}`).StatusCode != 400 {
			t.Fatal("invalid channel accepted")
		}
	}
	if managementCall(t, "PATCH", "/keys/channel", `{"id":"missing","channel":"vibe"}`).StatusCode != 404 {
		t.Fatal("missing key accepted")
	}
}

func TestDualQuotasSurviveIdentityFailureAndPartialBudget(t *testing.T) {
	isolatedState(t)
	oldTime := time.Now().Add(-time.Hour).UTC()
	state.Keys = []apiKey{{ID: "key", Secret: "secret", Channel: "studio", Source: "chat", Quotas: &keyQuotas{Vibe: keyQuota{Kind: "chat", Available: true, Limit: 255, Remaining: 200, UpdatedAt: oldTime}}}}
	calls := 0
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		calls++
		req := payload.(upstreamRequest)
		if strings.HasSuffix(req.URL, "whoami") {
			return nil, errors.New("identity offline")
		}
		body := `{"vibe_budget":null,"api_budget":{"initial_budget":0,"usage_percentage":0,"currency":"EUR","payg_enabled":true}}`
		if strings.HasSuffix(req.URL, "subscription") {
			body = `{"plan_name":"PRO","payg_enabled":false,"vibe_payg_enabled":true}`
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(body)})
	}
	response := managementCall(t, "POST", "/keys/quota", `{"id":"key","force":true}`)
	var key safeKey
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &key) != nil || key.Quotas == nil || key.Source != "chat" || key.ActiveChannel != "studio" {
		t.Fatalf("refresh: %s", response.Body)
	}
	if !key.Quotas.Vibe.Available || key.Quotas.Vibe.Remaining != 200 || !key.Quotas.Vibe.UpdatedAt.Equal(oldTime) || key.Quotas.Vibe.Error == "" {
		t.Fatalf("lost previous Vibe pool: %+v", key.Quotas.Vibe)
	}
	if !key.Quotas.Studio.Available || key.Quotas.Studio.Limit != 0 || !key.Quotas.Studio.PayAsYouGo || key.Quotas.Studio.Error != "" {
		t.Fatalf("zero budget or per-pool PAYG lost: %+v", key.Quotas.Studio)
	}
	managementCall(t, "POST", "/keys/quota", `{"id":"key"}`)
	if calls != 3 {
		t.Fatal("dual quota refresh did not use cache")
	}
	stored, err := os.ReadFile(filepath.Join(os.Getenv("MISTRAL_BRIDGE_DATA_DIR"), "state.json"))
	if err != nil || !strings.Contains(string(stored), `"quotas"`) || strings.Contains(string(response.Body), "secret") {
		t.Fatal("quota persistence or credential masking failed")
	}
}

func TestQuotaRefreshRetainsConcurrentChannelSwitch(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "key", Secret: "secret", Source: "api", Channel: "studio"}}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(upstreamRequest)
		body := `{"plan_type":"api"}`
		if strings.HasSuffix(req.URL, "budget") {
			response := managementCall(t, "PATCH", "/keys/channel", `{"id":"key","channel":"vibe"}`)
			if response.StatusCode != 200 {
				t.Fatal(string(response.Body))
			}
			body = `{"vibe_budget":{"initial_budget":255,"usage_percentage":1},"api_budget":{"initial_budget":25.5,"usage_percentage":2}}`
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(body)})
	}
	response := managementCall(t, "POST", "/keys/quota", `{"id":"key","force":true}`)
	var key safeKey
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &key) != nil || key.Channel != "vibe" || key.Quota == nil || key.Quota.Limit != 255 || key.Source != "api" {
		t.Fatalf("refresh undid channel switch: %s", response.Body)
	}
}
