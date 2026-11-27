package bridge

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"time"
)

type usageView struct {
	usageTotals
	CacheRate           *float64 `json:"cache_rate"`
	TokenRate           *float64 `json:"token_rate"`
	AverageMS           *float64 `json:"average_duration_ms"`
	AverageFirstTokenMS *float64 `json:"average_first_token_ms"`
}

func viewUsage(t usageTotals) usageView {
	v := usageView{usageTotals: t}
	if t.CacheInput > 0 {
		rate := float64(t.Cached) * 100 / float64(t.CacheInput)
		v.CacheRate = &rate
	}
	if t.GenerationMS > 0 {
		rate := float64(t.SpeedOutput) * 1000 / float64(t.GenerationMS)
		v.TokenRate = &rate
	}
	if t.FirstTokenSamples > 0 {
		average := float64(t.FirstTokenMS) / float64(t.FirstTokenSamples)
		v.AverageFirstTokenMS = &average
	}
	if t.Requests > 0 {
		average := float64(t.DurationMS) / float64(t.Requests)
		v.AverageMS = &average
	}
	return v
}

// History uses UTC calendar dates with an exclusive end, intentionally separate
// from /logs' exact timestamps and text search. Never silently widen a range.
func queryHistoricalStats(query url.Values) ([]byte, error) {
	if query.Has("timezone") || query.Has("from") || query.Has("to") || query.Has("granularity") {
		return queryUsageTimeline(query)
	}
	for key := range query {
		switch key {
		case "from_day", "to_day", "model", "upstream_model", "key_id", "thinking", "status":
		default:
			return managementJSONStatus(400, map[string]string{"error": "历史统计只支持 UTC 日期及模型、凭证、思考强度、结果筛选"})
		}
	}
	from, to := query.Get("from_day"), query.Get("to_day")
	for _, date := range []string{from, to} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return managementJSONStatus(400, map[string]string{"error": "日期必须为 YYYY-MM-DD"})
			}
		}
	}
	if from != "" && to != "" && from >= to {
		return managementJSONStatus(400, map[string]string{"error": "结束日期必须晚于起始日期"})
	}
	logMu.Lock()
	defer logMu.Unlock()
	path, err := statePath()
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	s, err := ensureTelemetryLocked(path)
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	var total usageTotals
	days := map[string]*usageTotals{}
	models, upstreams := map[string]bool{}, map[string]bool{}
	keys := map[string]string{}
	keyTimes := map[string]time.Time{}
	for _, b := range s.data.Buckets {
		models[b.Model] = true
		upstreams[b.UpstreamModel] = true
		if b.KeyID != "" && !b.LastSeen.Before(keyTimes[b.KeyID]) {
			keys[b.KeyID] = b.KeyLabel
			keyTimes[b.KeyID] = b.LastSeen
		}
		if from != "" && (b.Day == "unknown" || b.Day < from) || to != "" && (b.Day == "unknown" || b.Day >= to) {
			continue
		}
		if value := query.Get("model"); value != "" && b.Model != value {
			continue
		}
		if value := query.Get("upstream_model"); value != "" && b.UpstreamModel != value {
			continue
		}
		if value := query.Get("key_id"); value != "" && b.KeyID != value {
			continue
		}
		if value := query.Get("thinking"); value != "" && b.Thinking != value {
			continue
		}
		if value := query.Get("status"); value != "" && b.Status != value {
			continue
		}
		total.add(b.usageTotals)
		if days[b.Day] == nil {
			days[b.Day] = &usageTotals{}
		}
		days[b.Day].add(b.usageTotals)
	}
	stateMu.RLock()
	for _, key := range state.Keys {
		if _, exists := keys[key.ID]; exists {
			keys[key.ID] = key.Label
		}
	}
	stateMu.RUnlock()
	type dailyView struct {
		Day string `json:"day"`
		usageView
	}
	daily := make([]dailyView, 0, len(days))
	for day, totals := range days {
		daily = append(daily, dailyView{day, viewUsage(*totals)})
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Day > daily[j].Day })
	return managementJSON(map[string]any{
		"stats": viewUsage(total), "daily": daily, "timezone": "UTC", "granularity": "day",
		"started_at": s.data.StartedAt, "epoch": s.data.Epoch, "incomplete": len(s.data.Reasons) > 0,
		"incomplete_reasons": s.data.Reasons, "models": models, "upstream_models": upstreams, "keys": keys,
	})
}

func telemetrySettings(method string, body []byte) ([]byte, error) {
	logMu.Lock()
	defer logMu.Unlock()
	path, err := statePath()
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	s, err := ensureTelemetryLocked(path)
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	if method == http.MethodPatch {
		config := s.config
		if err := json.Unmarshal(body, &config); err != nil {
			return managementJSONStatus(400, map[string]string{"error": "日志保留设置不是有效 JSON"})
		}
		if err := config.validate(); err != nil {
			return managementJSONStatus(400, map[string]string{"error": err.Error()})
		}
		raw, _ := json.Marshal(config)
		if err := atomicTelemetryFile(s.file("config.json"), raw); err != nil {
			return managementJSONStatus(500, map[string]string{"error": err.Error()})
		}
		s.config = config
		stateMu.RLock()
		rows := append([]RequestLog(nil), state.Logs...)
		stateMu.RUnlock()
		if err := s.retain(rows, true); err != nil {
			return managementJSONStatus(500, map[string]string{"error": telemetryErrorLocked(path, err).Error()})
		}
	}
	return managementJSON(s.config)
}

func resetHistoricalStats() ([]byte, error) {
	logMu.Lock()
	defer logMu.Unlock()
	path, err := statePath()
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	s, err := ensureTelemetryLocked(path)
	if err != nil {
		return managementJSONStatus(503, map[string]string{"error": err.Error()})
	}
	// Keep the journal high-water mark and known IDs. Old retained rows must not
	// be reimported into this new statistics epoch after a restart/downgrade.
	s.data.Epoch = randomID()
	s.data.StartedAt = time.Now().UTC()
	s.data.Buckets = map[string]*usageBucket{}
	s.data.Minutes = map[string]*usageBucket{}
	s.data.Reasons = nil
	if err := s.checkpoint(); err != nil {
		return managementJSONStatus(500, map[string]string{"error": telemetryErrorLocked(path, err).Error()})
	}
	return managementJSON(map[string]bool{"ok": true})
}

func prepareLogRetention() (retentionConfig, string) {
	logMu.Lock()
	defer logMu.Unlock()
	path, err := statePath()
	if err != nil {
		return defaultRetention(), err.Error()
	}
	s, err := ensureTelemetryLocked(path)
	if err != nil {
		return defaultRetention(), err.Error()
	}
	stateMu.RLock()
	rows := append([]RequestLog(nil), state.Logs...)
	stateMu.RUnlock()
	if err := s.retain(rows, false); err != nil {
		return s.config, telemetryErrorLocked(path, err).Error()
	}
	return s.config, ""
}
