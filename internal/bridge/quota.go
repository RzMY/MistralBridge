package bridge

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

type keyQuotas struct {
	Vibe      keyQuota  `json:"vibe"`
	Studio    keyQuota  `json:"studio"`
	UpdatedAt time.Time `json:"updated_at"`
}

func keySource(key apiKey) string {
	if key.Source != "" {
		return key.Source
	}
	if key.Quota != nil {
		return key.Quota.Kind
	}
	return ""
}

func activeChannel(key apiKey) string {
	if key.Channel == "vibe" || key.Channel == "studio" {
		return key.Channel
	}
	switch keySource(key) {
	case "chat":
		return "vibe"
	case "api":
		return "studio"
	}
	return ""
}

func selectedQuota(key apiKey) *keyQuota {
	if key.Quotas != nil {
		switch activeChannel(key) {
		case "vibe":
			return &key.Quotas.Vibe
		case "studio":
			return &key.Quotas.Studio
		}
	}
	if key.Quota != nil && ((activeChannel(key) == "vibe" && key.Quota.Kind == "chat") || (activeChannel(key) == "studio" && key.Quota.Kind == "api")) {
		return key.Quota
	}
	return nil
}

func migrateKeyQuota(key *apiKey) {
	if key.Source == "" {
		key.Source = keySource(*key)
	}
	if key.Quotas == nil && key.Quota != nil {
		key.Quotas = &keyQuotas{Vibe: keyQuota{Kind: "chat"}, Studio: keyQuota{Kind: "api"}}
		if key.Quota.Kind == "chat" {
			key.Quotas.Vibe = *key.Quota
		} else if key.Quota.Kind == "api" {
			key.Quotas.Studio = *key.Quota
		}
	}
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
	key, err := refreshKeyQuotas(input.ID, input.Force, callbackID)
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicKey(key))
}

// Both the plugin console and CPA's quota provider share persistence and cache.
func refreshKeyQuotas(id string, force bool, callbackID string, nativeProxy ...string) (apiKey, error) {
	lock, _ := quotaLocks.LoadOrStore(id, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	key, ok := getKey(id)
	if !ok {
		return apiKey{}, &statusError{404, "凭证不存在"}
	}
	if key.Quotas != nil && time.Since(key.Quotas.UpdatedAt) < 5*time.Minute && !force {
		return key, nil
	}
	if len(nativeProxy) > 0 {
		key.NativeProxyURL = &nativeProxy[0]
	}
	quotas, source, err := fetchKeyQuotas(key, callbackID)
	if err != nil {
		quotas.Vibe.Error, quotas.Studio.Error = err.Error(), err.Error()
	}
	// Keep each pool's last successful value and timestamp independently.
	merge := func(fresh keyQuota, old *keyQuota) keyQuota {
		if fresh.Error != "" && !fresh.Available && old != nil && old.Available {
			previous := *old
			previous.Error = fresh.Error
			return previous
		}
		return fresh
	}
	if key.Quotas != nil {
		quotas.Vibe = merge(quotas.Vibe, &key.Quotas.Vibe)
		quotas.Studio = merge(quotas.Studio, &key.Quotas.Studio)
	} else if key.Quota != nil {
		if key.Quota.Kind == "chat" {
			quotas.Vibe = merge(quotas.Vibe, key.Quota)
		} else if key.Quota.Kind == "api" {
			quotas.Studio = merge(quotas.Studio, key.Quota)
		}
	}
	if err := mutateState(func(s *pluginState) error {
		for i := range s.Keys {
			if s.Keys[i].ID == key.ID {
				if s.Keys[i].Secret != key.Secret || s.Keys[i].BaseURL != key.BaseURL {
					return &statusError{409, "凭证已更新，请重新刷新额度"}
				}
				if source != "" {
					s.Keys[i].Source = source
				} else if s.Keys[i].Source == "" {
					s.Keys[i].Source = keySource(key)
				}
				s.Keys[i].Quotas = &quotas
				s.Keys[i].Quota = selectedQuota(s.Keys[i])
				key = s.Keys[i]
				return nil
			}
		}
		return &statusError{404, "凭证已删除"}
	}); err != nil {
		return apiKey{}, err
	}
	return key, nil
}

func quotaJSON(endpoint string, key apiKey, callbackID string, result any) error {
	proxyURL, err := credentialProxyURL(currentState(), key)
	if err != nil {
		return err
	}
	raw, err := doUpstreamHTTP(nil, upstreamRequest{Method: http.MethodGet, URL: endpoint, Headers: http.Header{"Authorization": {"Bearer " + key.Secret}, "Accept": {"application/json"}}, HostCallbackID: callbackID}, proxyURL)
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
	quotas, source, err := fetchKeyQuotas(key, callbackID)
	key.Quotas = &quotas
	if source != "" {
		key.Source = source
	}
	quota := selectedQuota(key)
	if quota == nil {
		if err != nil {
			return keyQuota{}, err
		}
		return keyQuota{}, fmt.Errorf("尚未识别凭证来源，请选择使用渠道")
	}
	if err == nil && quota.Error != "" {
		err = fmt.Errorf("%s", quota.Error)
	}
	return *quota, err
}

func fetchKeyQuotas(key apiKey, callbackID string) (keyQuotas, string, error) {
	proxyURL, proxyErr := credentialProxyURL(currentState(), key)
	key.NativeProxyURL = &proxyURL
	now := time.Now().UTC()
	quotas := keyQuotas{UpdatedAt: now, Vibe: keyQuota{Kind: "chat", UpdatedAt: now}, Studio: keyQuota{Kind: "api", UpdatedAt: now}}
	if proxyErr != nil {
		return quotas, "", proxyErr
	}
	base := strings.TrimRight(key.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Host != "api.mistral.ai" && parsed.Host != "api.eu.mistral.ai" && parsed.Host != "api.us.mistral.ai") || parsed.Scheme != "https" {
		return quotas, "", fmt.Errorf("此 API 地址未提供 Mistral 官方额度查询")
	}
	var identity struct {
		Kind string `json:"plan_type"`
		Name string `json:"plan_name"`
	}
	// Billing is useful even when the console identity service is unavailable.
	source := ""
	if quotaJSON("https://console.mistral.ai/api/vibe/whoami", key, callbackID, &identity) == nil {
		source = strings.ToLower(strings.TrimSpace(identity.Kind))
	}
	var budget map[string]json.RawMessage
	if err := quotaJSON(base+"/billing/budget", key, callbackID, &budget); err != nil {
		return quotas, source, err
	}
	var subscription struct {
		Plan     string `json:"plan_name"`
		Payg     bool   `json:"payg_enabled"`
		VibePayg bool   `json:"vibe_payg_enabled"`
	}
	_ = quotaJSON(base+"/billing/subscription", key, callbackID, &subscription)
	plan := subscription.Plan
	if plan == "" {
		plan = identity.Name
	}
	quotas.Vibe = parseBudget(budget["vibe_budget"], "chat", plan, subscription.VibePayg, now)
	quotas.Studio = parseBudget(budget["api_budget"], "api", plan, subscription.Payg, now)
	return quotas, source, nil
}

func parseBudget(raw json.RawMessage, kind, plan string, payg bool, now time.Time) keyQuota {
	quota := keyQuota{Kind: kind, Plan: plan, PayAsYouGo: payg, UpdatedAt: now}
	var detail struct {
		Limit      *float64 `json:"initial_budget"`
		Percentage *float64 `json:"usage_percentage"`
		Currency   string   `json:"currency"`
		ResetAt    string   `json:"reset_at"`
		Payg       *bool    `json:"payg_enabled"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.Limit == nil || detail.Percentage == nil || *detail.Limit < 0 {
		quota.Error = "上游未提供可用的月度预算"
		return quota
	}
	quota.Available = true
	quota.Limit = *detail.Limit
	quota.UsedPercent = max(0, *detail.Percentage)
	quota.Used = quota.Limit * quota.UsedPercent / 100
	quota.Remaining = max(0, quota.Limit-quota.Used)
	quota.Currency = detail.Currency
	quota.ResetAt = detail.ResetAt
	if detail.Payg != nil {
		quota.PayAsYouGo = *detail.Payg
	}
	return quota
}
