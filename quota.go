package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type keyQuota struct {
	Plan        string    `json:"plan,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	Available   bool      `json:"available"`
	Limit       float64   `json:"limit"`
	Used        float64   `json:"used"`
	Remaining   float64   `json:"remaining"`
	UsedPercent float64   `json:"used_percent"`
	Currency    string    `json:"currency,omitempty"`
	ResetAt     string    `json:"reset_at,omitempty"`
	PayAsYouGo  bool      `json:"pay_as_you_go"`
	UpdatedAt   time.Time `json:"updated_at"`
	Error       string    `json:"error,omitempty"`
}

// Coalesce concurrent refreshes per credential without holding the state lock during HTTP.
var quotaLocks sync.Map

func refreshQuota(body []byte, callbackID string) ([]byte, error) {
	var input struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	if json.Unmarshal(body, &input) != nil || input.ID == "" {
		return managementJSONStatus(400, map[string]string{"error": "请选择凭证"})
	}
	lock, _ := quotaLocks.LoadOrStore(input.ID, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	key, ok := getKey(input.ID)
	if !ok {
		return managementJSONStatus(404, map[string]string{"error": "凭证不存在"})
	}
	if key.Quota != nil && time.Since(key.Quota.UpdatedAt) < 5*time.Minute && !input.Force {
		return managementJSON(publicKey(key))
	}
	quota, err := fetchQuota(key, callbackID)
	if err != nil {
		quota.Error = err.Error()
		if key.Quota != nil {
			old := *key.Quota
			old.Error = err.Error()
			quota = old
		}
	}
	if err := mutateState(func(s *pluginState) error {
		for i := range s.Keys {
			if s.Keys[i].ID == key.ID {
				if s.Keys[i].Secret != key.Secret || s.Keys[i].BaseURL != key.BaseURL {
					return &statusError{409, "凭证已更新，请重新刷新额度"}
				}
				s.Keys[i].Quota = &quota
				key = s.Keys[i]
				return nil
			}
		}
		return &statusError{404, "凭证已删除"}
	}); err != nil {
		return mutationError(err)
	}
	return managementJSON(publicKey(key))
}

func quotaJSON(endpoint string, key apiKey, callbackID string, result any) error {
	raw, err := hostCall("host.http.do", upstreamRequest{Method: http.MethodGet, URL: endpoint, Headers: http.Header{"Authorization": {"Bearer " + key.Secret}, "Accept": {"application/json"}}, HostCallbackID: callbackID})
	if err != nil {
		return fmt.Errorf("额度服务暂时无法连接")
	}
	var response upstreamResponse
	if json.Unmarshal(raw, &response) != nil {
		return fmt.Errorf("额度响应无效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("额度暂不可用（HTTP %d）", response.StatusCode)
	}
	if json.Unmarshal(response.Body, result) != nil {
		return fmt.Errorf("额度数据格式无效")
	}
	return nil
}

func fetchQuota(key apiKey, callbackID string) (keyQuota, error) {
	quota := keyQuota{UpdatedAt: time.Now().UTC()}
	base := strings.TrimRight(key.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Host != "api.mistral.ai" && parsed.Host != "api.eu.mistral.ai" && parsed.Host != "api.us.mistral.ai") || parsed.Scheme != "https" {
		return quota, fmt.Errorf("此 API 地址未提供 Mistral 官方额度查询")
	}
	var identity struct {
		Kind string `json:"plan_type"`
		Name string `json:"plan_name"`
	}
	if err := quotaJSON("https://console.mistral.ai/api/vibe/whoami", key, callbackID, &identity); err != nil {
		return quota, err
	}
	quota.Kind = strings.ToLower(identity.Kind)
	quota.Plan = identity.Name
	if quota.Kind != "chat" && quota.Kind != "api" {
		return quota, fmt.Errorf("该类型凭证未提供月度预算")
	}
	var budget map[string]json.RawMessage
	if err := quotaJSON(base+"/billing/budget", key, callbackID, &budget); err != nil {
		return quota, err
	}
	field := "vibe_budget"
	if quota.Kind == "api" {
		field = "api_budget"
	}
	var detail struct {
		Limit      *float64 `json:"initial_budget"`
		Percentage *float64 `json:"usage_percentage"`
		Currency   string   `json:"currency"`
		ResetAt    string   `json:"reset_at"`
		Payg       *bool    `json:"payg_enabled"`
	}
	if json.Unmarshal(budget[field], &detail) != nil || detail.Limit == nil || detail.Percentage == nil || *detail.Limit <= 0 {
		return quota, fmt.Errorf("上游未提供可用的月度预算")
	}
	quota.Available = true
	quota.Limit = *detail.Limit
	quota.UsedPercent = max(0, *detail.Percentage)
	quota.Used = quota.Limit * quota.UsedPercent / 100
	quota.Remaining = max(0, quota.Limit-quota.Used)
	quota.Currency = detail.Currency
	quota.ResetAt = detail.ResetAt
	var subscription struct {
		Plan     string `json:"plan_name"`
		Payg     bool   `json:"payg_enabled"`
		VibePayg bool   `json:"vibe_payg_enabled"`
	}
	if quotaJSON(base+"/billing/subscription", key, callbackID, &subscription) == nil {
		quota.Plan = subscription.Plan
		quota.PayAsYouGo = subscription.VibePayg
		if quota.Kind == "api" {
			quota.PayAsYouGo = subscription.Payg
		}
	}
	if detail.Payg != nil {
		quota.PayAsYouGo = *detail.Payg
	}
	return quota, nil
}
