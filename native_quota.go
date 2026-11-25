package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Quota RPCs use snake_case, unlike the auth/executor capability requests.
type nativeQuotaRequest struct {
	AuthID         string                   `json:"auth_id"`
	Provider       string                   `json:"provider"`
	StorageJSON    []byte                   `json:"storage_json"`
	Metadata       map[string]any           `json:"metadata"`
	Host           struct{ AuthDir string } `json:"host"`
	HostCallbackID string                   `json:"host_callback_id"`
}

type nativeQuotaSubscription struct {
	Plan string `json:"plan,omitempty"`
}

type nativeQuotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type nativeQuotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description"`
}

type nativeQuotaGroup struct {
	DisplayName string              `json:"displayName"`
	Buckets     []nativeQuotaBucket `json:"buckets,omitempty"`
}

type nativeQuotaResponse struct {
	Subscription *nativeQuotaSubscription `json:"subscription,omitempty"`
	Summary      []nativeQuotaMetric      `json:"summary,omitempty"`
	Groups       []nativeQuotaGroup       `json:"groups"`
}

func describeNativeQuota() ([]byte, error) {
	return okEnvelope(map[string]any{"supported_providers": []string{pluginID}, "display_name": "Mistral Vibe / Studio", "supports_reset": false})
}

func fetchNativeQuota(raw []byte) ([]byte, error) {
	var req nativeQuotaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, &statusError{400, "额度请求无效"}
	}
	if req.Provider != pluginID || req.AuthID == "" {
		return nil, &statusError{400, "请选择 Mistral 凭证"}
	}
	configureDataDirectory(req.Host.AuthDir)
	doc, err := authDocument(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, &statusError{400, "Mistral 凭证数据无效"}
	}
	var key apiKey
	if id := authString(doc, "bridge_key_id"); id != "" {
		// Quota remains inspectable for disabled credentials; this is not admission.
		key, err = refreshKeyQuotas(id, false, req.HostCallbackID, authString(doc, "proxy_url"))
		if err != nil {
			return nil, err
		}
	} else {
		key = apiKey{ID: req.AuthID, Secret: strings.TrimSpace(authString(doc, "api_key")), BaseURL: authString(doc, "mistral_base_url"), Channel: authString(doc, "channel"), Source: authString(doc, "source")}
		if key.Secret == "" {
			return nil, &statusError{401, "Mistral 凭证缺少 API Key"}
		}
		proxyURL := authString(doc, "proxy_url")
		key.NativeProxyURL = &proxyURL
		quotas, source, fetchErr := fetchKeyQuotas(key, req.HostCallbackID)
		if fetchErr != nil {
			return nil, &statusError{502, fetchErr.Error()}
		}
		key.Quotas = &quotas
		if source != "" {
			key.Source = source
		}
	}
	result, err := normalizeNativeQuota(key)
	if err != nil {
		return nil, err
	}
	return okEnvelope(result)
}

func normalizeNativeQuota(key apiKey) (nativeQuotaResponse, error) {
	migrateKeyQuota(&key)
	result := nativeQuotaResponse{Groups: []nativeQuotaGroup{}}
	if key.Quotas == nil {
		return result, &statusError{502, "Mistral 尚未提供额度数据"}
	}
	pools := []struct {
		id, name string
		quota    keyQuota
	}{
		{"vibe", "Vibe", key.Quotas.Vibe},
		{"studio", "Studio / API", key.Quotas.Studio},
	}
	if activeChannel(key) == "studio" {
		pools[0], pools[1] = pools[1], pools[0]
	}
	available := false
	var failures []string
	for _, pool := range pools {
		q := pool.quota
		name := pool.name
		if pool.id == activeChannel(key) {
			name += " · 当前渠道"
		}
		group := nativeQuotaGroup{DisplayName: name}
		if !q.Available {
			message := q.Error
			if message == "" {
				message = "上游未提供可用的月度预算"
			}
			group.DisplayName += " · 不可用：" + message
			failures = append(failures, pool.name+"："+message)
			result.Groups = append(result.Groups, group)
			continue
		}
		available = true
		if result.Subscription == nil && q.Plan != "" {
			result.Subscription = &nativeQuotaSubscription{Plan: q.Plan}
		}
		fraction := 0.0
		if q.Limit > 0 {
			fraction = min(1, max(0, 1-q.UsedPercent/100))
		}
		unit := strings.ToUpper(strings.TrimSpace(q.Currency))
		description := fmt.Sprintf("剩余 %.2f / %.2f %s；已用 %.2f %s", q.Remaining, q.Limit, unit, q.Used, unit)
		if q.PayAsYouGo {
			description += "；按量付费已开启"
		} else {
			description += "；按量付费未开启"
		}
		if !q.UpdatedAt.IsZero() {
			description += "；数据时间 " + q.UpdatedAt.UTC().Format(time.RFC3339)
		}
		metricName := pool.name
		if q.Error != "" {
			group.DisplayName += " · 历史数据"
			metricName += "（历史数据）"
			description += "；本次刷新失败：" + q.Error
		}
		if keySource(key) == "chat" && pool.id == "studio" && activeChannel(key) == "studio" {
			description += "；Vibe 来源 Key 仍可能扣 Vibe 额度，以上游计费为准"
		}
		group.Buckets = []nativeQuotaBucket{{Window: "monthly", RemainingFraction: fraction, ResetTime: q.ResetAt, Description: description}}
		result.Groups = append(result.Groups, group)
		for _, metric := range []struct {
			id, label string
			value     float64
		}{
			{"remaining", "剩余额度", q.Remaining}, {"used", "已用额度", q.Used}, {"limit", "月度额度", q.Limit},
		} {
			item := nativeQuotaMetric{Key: pool.id + "_" + metric.id, Label: metricName + " " + metric.label, Value: metric.value, Format: "number"}
			if len(unit) == 3 && unit[0] >= 'A' && unit[0] <= 'Z' && unit[1] >= 'A' && unit[1] <= 'Z' && unit[2] >= 'A' && unit[2] <= 'Z' {
				item.Format, item.Currency = "currency", unit
			}
			result.Summary = append(result.Summary, item)
		}
	}
	if !available {
		return result, &statusError{502, strings.Join(failures, "；")}
	}
	// CPA panels may omit groups without buckets. Keep partial failures visible
	// alongside a usable pool instead of inventing an exhausted bucket.
	if len(failures) > 0 {
		for i := range result.Groups {
			for j := range result.Groups[i].Buckets {
				result.Groups[i].Buckets[j].Description += "；其他额度池不可用：" + strings.Join(failures, "；")
			}
		}
	}
	return result, nil
}
