package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func nativeQuotaInput(id string, metadata map[string]any) []byte {
	raw, _ := json.Marshal(nativeQuotaRequest{AuthID: id, Provider: pluginID, Metadata: metadata, HostCallbackID: "quota-context"})
	return raw
}

func decodeNativeQuota(t *testing.T, raw []byte) nativeQuotaResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("invalid quota envelope: %s", raw)
	}
	var result nativeQuotaResponse
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNativeQuotaDiscovery(t *testing.T) {
	isolatedState(t)
	hostCall = func(string, any) (json.RawMessage, error) {
		t.Fatal("quota discovery must not contact upstream")
		return nil, nil
	}
	raw, err := registration()
	if err != nil || !strings.Contains(string(raw), `"quota_provider":true`) {
		t.Fatal("quota provider not registered")
	}
	raw, err = HandleMethod("quota.identifier", nil)
	if err != nil || !strings.Contains(string(raw), pluginID) {
		t.Fatal("identifier missing")
	}
	raw, err = HandleMethod("quota.describe", nil)
	if err != nil || !strings.Contains(string(raw), `"supports_reset":false`) {
		t.Fatal("reset capability incorrect")
	}
	raw, err = HandleMethod("quota.reset", nil)
	if err != nil || !strings.Contains(string(raw), `"success":false`) {
		t.Fatal("upstream reset falsely supported")
	}
}

func TestNativeQuotaRefreshAndSharedCache(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "one", Secret: "mock-key", Channel: "vibe", Enabled: false}}
	var calls atomic.Int32
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		calls.Add(1)
		req := payload.(upstreamRequest)
		if method != "host.http.do" || req.Method != http.MethodGet || req.HostCallbackID != "quota-context" || req.Headers.Get("Authorization") != "Bearer mock-key" {
			t.Fatal("quota callback context or identity lost")
		}
		body := `{"plan_type":"chat"}`
		if strings.HasSuffix(req.URL, "budget") {
			body = `{"vibe_budget":{"initial_budget":200,"usage_percentage":25,"currency":"EUR","reset_at":"2026-12-01T00:00:00Z"},"api_budget":{"initial_budget":20,"usage_percentage":50,"currency":"EUR"}}`
		}
		if strings.HasSuffix(req.URL, "subscription") {
			body = `{"plan_name":"PRO","vibe_payg_enabled":true}`
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(body)})
	}
	input := nativeQuotaInput("native-one", map[string]any{"bridge_key_id": "one", "disabled": true})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fetchNativeQuota(input); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 3 {
		t.Fatalf("concurrent native refresh sent %d calls", calls.Load())
	}
	raw, err := fetchNativeQuota(input)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeNativeQuota(t, raw)
	if len(result.Groups) != 2 || result.Subscription.Plan != "PRO" || len(result.Summary) != 6 {
		t.Fatalf("normalized quota %+v", result)
	}
	vibe := result.Groups[0]
	if vibe.DisplayName != "Vibe · 当前渠道" || vibe.Buckets[0].RemainingFraction != .75 || vibe.Buckets[0].ResetTime != "2026-12-01T00:00:00Z" || !strings.Contains(vibe.Buckets[0].Description, "按量付费已开启") {
		t.Fatalf("Vibe %+v", vibe)
	}
	if result.Summary[0].Value != 150 || result.Summary[0].Currency != "EUR" {
		t.Fatalf("summary %+v", result.Summary)
	}
	if strings.Contains(string(raw), "mock-key") || strings.Contains(string(raw), "Bearer") {
		t.Fatal("quota response leaks credential")
	}
	response := managementCall(t, "POST", "/keys/quota", `{"id":"one"}`)
	if response.StatusCode != 200 || calls.Load() != 3 || currentState().Keys[0].Quotas.Vibe.Remaining != 150 {
		t.Fatal("native and console caches diverged")
	}
	managementCall(t, "PATCH", "/keys/channel", `{"id":"one","channel":"studio"}`)
	raw, err = fetchNativeQuota(input)
	if err != nil {
		t.Fatal(err)
	}
	result = decodeNativeQuota(t, raw)
	if result.Groups[0].DisplayName != "Studio / API · 当前渠道" || !strings.Contains(result.Groups[0].Buckets[0].Description, "仍可能扣 Vibe") || calls.Load() != 3 {
		t.Fatal("channel switch not reflected in native quota")
	}
}

func TestNativeQuotaFailureStalenessAndZeroPAYG(t *testing.T) {
	old := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	key := apiKey{Channel: "vibe", Quotas: &keyQuotas{Vibe: keyQuota{Available: true, Limit: 100, Remaining: 80, Used: 20, UsedPercent: 20, Plan: "PRO", UpdatedAt: old, Error: "HTTP 503"}, Studio: keyQuota{Error: "not provided"}}}
	result, err := normalizeNativeQuota(key)
	if err != nil || len(result.Summary) != 3 || len(result.Groups[1].Buckets) != 0 || !strings.Contains(result.Groups[0].DisplayName, "历史数据") || !strings.Contains(result.Groups[0].Buckets[0].Description, "2026-11-01T00:00:00Z") || !strings.Contains(result.Groups[0].Buckets[0].Description, "HTTP 503") {
		t.Fatalf("stale/unknown quota %+v %v", result, err)
	}
	key.Quotas.Studio = keyQuota{Available: true, Limit: 0, UsedPercent: 0, PayAsYouGo: true}
	result, err = normalizeNativeQuota(key)
	if err != nil || result.Groups[1].Buckets[0].RemainingFraction != 0 || !strings.Contains(result.Groups[1].Buckets[0].Description, "按量付费已开启") {
		t.Fatal("zero allocation mislabeled as unlimited quota")
	}
	key.Quotas.Vibe.Available = false
	key.Quotas.Studio.Available = false
	if _, err = normalizeNativeQuota(key); err == nil {
		t.Fatal("missing quotas reported as success")
	}
	key.Quotas.Vibe = keyQuota{Available: true, Limit: 10, UsedPercent: 120, Remaining: 0}
	result, err = normalizeNativeQuota(key)
	if err != nil || result.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Fatal("over-budget fraction not clamped")
	}
}

func TestNativeQuotaStandaloneAndForeignProvider(t *testing.T) {
	isolatedState(t)
	count := 0
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		count++
		req := payload.(upstreamRequest)
		if req.Headers.Get("Authorization") != "Bearer standalone-mock" {
			t.Fatal("wrong standalone key")
		}
		body := `{"plan_type":"api"}`
		if strings.HasSuffix(req.URL, "budget") {
			body = `{"vibe_budget":null,"api_budget":{"initial_budget":50,"usage_percentage":20}}`
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(body)})
	}
	raw, err := fetchNativeQuota(nativeQuotaInput("direct.json", map[string]any{"api_key": "standalone-mock", "channel": "studio"}))
	if err != nil || decodeNativeQuota(t, raw).Summary[0].Value != 40 || count != 3 || len(currentState().Keys) != 0 {
		t.Fatalf("standalone quota %s %v", raw, err)
	}
	for _, input := range [][]byte{[]byte(`{"auth_id":"foreign","provider":"other","metadata":{"api_key":"standalone-mock"}}`), nativeQuotaInput("missing", map[string]any{"bridge_key_id": "missing"})} {
		if _, err := fetchNativeQuota(input); err == nil {
			t.Fatal("foreign/missing identity accepted")
		}
	}
	if count != 3 {
		t.Fatal("invalid identity contacted upstream")
	}
	_, err = fetchNativeQuota(nativeQuotaInput("custom", map[string]any{"api_key": "standalone-mock", "mistral_base_url": "http://127.0.0.1/v1"}))
	var status *statusError
	if !errors.As(err, &status) || status.Status != 502 || count != 3 {
		t.Fatal("unsupported endpoint contacted upstream or reported fabricated quota")
	}
}
