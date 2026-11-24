package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRequestLogs = 10000

type requestTrace struct {
	entry     RequestLog
	started   time.Time
	secret    string
	debug     *debugExchange
	debugBody bytes.Buffer
}

func newRequestTrace(req rpcExecutorRequest) *requestTrace {
	now := time.Now()
	trace := &requestTrace{started: now, entry: RequestLog{ID: randomID(), Model: req.Model, Stream: req.Stream, CreatedAt: now.UTC(), Status: "ok", HTTPStatus: 200}}
	if debugModeEnabled() {
		trace.debug = &debugExchange{ID: trace.entry.ID, CreatedAt: now.UTC()}
	}
	return trace
}

func (t *requestTrace) resolved(key apiKey, model modelMapping, body []byte) {
	t.entry.UpstreamModel = model.UpstreamModel
	t.entry.KeyID = key.ID
	t.entry.KeyLabel = key.Label
	t.entry.KeyMasked = publicKey(key).Masked
	t.secret = key.Secret
	if t.debug != nil {
		t.debug.secret = key.Secret
	}
	var request struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	_ = json.Unmarshal(body, &request)
	t.entry.Thinking = request.ReasoningEffort
}

func (t *requestTrace) captureRequest(method, endpoint string, headers http.Header, body []byte) {
	if t.debug == nil {
		return
	}
	t.debug.Request = debugMessage{Method: method, URL: endpoint, Headers: headers.Clone(), Body: string(body)}
}

func (t *requestTrace) recordDroppedFields(report *requestDropReport) {
	if report != nil {
		t.entry.DroppedCount = report.Count
		t.entry.DroppedFields = append([]droppedRequestField(nil), report.Fields...)
	}
}

func (t *requestTrace) captureResponse(status int, headers http.Header, body []byte, complete bool) {
	if t.debug == nil {
		return
	}
	t.debug.Response.StatusCode = status
	t.debug.Response.Headers = headers.Clone()
	t.debugBody.Reset()
	_, _ = t.debugBody.Write(body)
	t.debug.Response.Complete = complete
}

func (t *requestTrace) captureResponseHead(status int, headers http.Header) {
	if t.debug == nil {
		return
	}
	t.debug.Response.StatusCode = status
	t.debug.Response.Headers = headers.Clone()
}

func (t *requestTrace) captureResponseChunk(body []byte) {
	if t.debug == nil || len(body) == 0 {
		return
	}
	_, _ = t.debugBody.Write(body)
}

func (t *requestTrace) completeResponse() {
	if t.debug != nil {
		t.debug.Response.Complete = true
	}
}

func (t *requestTrace) captureResponseError(err error) {
	if t.debug != nil && err != nil {
		t.debug.Response.Error = err.Error()
	}
}

func (t *requestTrace) capturesDebug() bool {
	return t.debug != nil
}

func (t *requestTrace) observe(line []byte) {
	if t.entry.FirstTokenMS != nil {
		return
	}
	data := strings.TrimSpace(string(line))
	if !strings.HasPrefix(data, "data:") {
		return
	}
	var event struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(data, "data:"))), &event) != nil {
		return
	}
	for _, choice := range event.Choices {
		for _, name := range []string{"content", "reasoning_content", "tool_calls"} {
			value := string(choice.Delta[name])
			if value != "" && value != "null" && value != `""` && value != "[]" {
				elapsed := time.Since(t.started).Milliseconds()
				t.entry.FirstTokenMS = &elapsed
				return
			}
		}
	}
}

func (t *requestTrace) finish(usage map[string]int, err error) {
	t.entry.DurationMS = time.Since(t.started).Milliseconds()
	if t.debug != nil {
		t.debug.DurationMS = t.entry.DurationMS
		t.debug.Response.Body = t.debugBody.String()
		t.debug.secret = t.secret
		if err != nil && t.debug.Response.Error == "" && t.debug.Response.StatusCode == 0 {
			t.captureResponseError(err)
		}
		if saveDebugLog(t.debug, t.secret) == nil {
			t.entry.DebugAvailable = true
		}
	}
	if usage != nil {
		t.entry.UsageKnown = true
		t.entry.InputTokens = usage["prompt_tokens"]
		t.entry.OutputTokens = usage["completion_tokens"]
		t.entry.TotalTokens = usage["total_tokens"]
		if t.entry.TotalTokens == 0 {
			t.entry.TotalTokens = t.entry.InputTokens + t.entry.OutputTokens
		}
		if cached, exists := usage["cached_tokens"]; exists {
			t.entry.CachedTokens = &cached
		}
	}
	if err != nil {
		t.entry.Status = "error"
		t.entry.HTTPStatus = 502
		var e *statusError
		if errors.As(err, &e) {
			t.entry.HTTPStatus = e.Status
		}
		message := err.Error()
		if t.secret != "" {
			message = strings.ReplaceAll(message, t.secret, "[redacted]")
		}
		if len(message) > 600 {
			message = message[:600]
		}
		t.entry.Error = message
	}
	_ = appendRequestLog(t.entry)
}

type logStats struct {
	Requests     int      `json:"requests"`
	Input        int64    `json:"input_tokens"`
	Output       int64    `json:"output_tokens"`
	Total        int64    `json:"total_tokens"`
	Cached       int64    `json:"cached_tokens"`
	CacheRate    *float64 `json:"cache_rate"`
	TokenRate    *float64 `json:"token_rate"`
	CacheSamples int      `json:"cache_samples"`
	SpeedSamples int      `json:"speed_samples"`
}

type logFilter struct {
	Search, Status, KeyID, Model, Thinking string
	From, To                               time.Time
	Page, PageSize                         int
}

func queryRequestLogs(query url.Values) ([]byte, error) {
	filter := logFilter{Search: strings.ToLower(strings.TrimSpace(query.Get("search"))), Status: query.Get("status"), KeyID: query.Get("key_id"), Model: query.Get("model"), Thinking: query.Get("thinking"), Page: 1, PageSize: 50}
	var err error
	if value := query.Get("from"); value != "" {
		filter.From, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return managementJSONStatus(400, map[string]string{"error": "起始时间无效"})
		}
	}
	if value := query.Get("to"); value != "" {
		filter.To, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return managementJSONStatus(400, map[string]string{"error": "结束时间无效"})
		}
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.To.Before(filter.From) {
		return managementJSONStatus(400, map[string]string{"error": "结束时间不能早于起始时间"})
	}
	if n, err := strconv.Atoi(query.Get("page")); err == nil && n > 0 {
		filter.Page = min(n, maxRequestLogs)
	}
	if n, err := strconv.Atoi(query.Get("page_size")); err == nil && n > 0 {
		filter.PageSize = min(n, 100)
	}
	stateMu.RLock()
	logs := append([]RequestLog(nil), state.Logs...)
	stateMu.RUnlock()
	return managementJSON(filterRequestLogs(logs, filter))
}

func filterRequestLogs(logs []RequestLog, filter logFilter) map[string]any {
	var stats logStats
	matches := make([]RequestLog, 0)
	var cacheInput, speedOutput, generationMS int64
	models := map[string]bool{}
	keys := map[string]string{}
	for i := len(logs) - 1; i >= 0; i-- {
		row := logs[i]
		models[row.Model] = true
		if row.KeyID != "" {
			keys[row.KeyID] = row.KeyLabel
		}
		if !filter.From.IsZero() && row.CreatedAt.Before(filter.From) {
			continue
		}
		if !filter.To.IsZero() && !row.CreatedAt.Before(filter.To) {
			continue
		}
		if filter.Status != "" && row.Status != filter.Status {
			continue
		}
		if filter.KeyID != "" && row.KeyID != filter.KeyID {
			continue
		}
		if filter.Model != "" && row.Model != filter.Model {
			continue
		}
		if filter.Thinking != "" && row.Thinking != filter.Thinking {
			continue
		}
		searchable := strings.ToLower(strings.Join([]string{row.ID, row.Model, row.UpstreamModel, row.KeyLabel, row.KeyMasked, row.Error, row.Thinking, strconv.Itoa(row.HTTPStatus)}, " "))
		for _, field := range row.DroppedFields {
			searchable += " " + strings.ToLower(field.Path+" "+field.Value)
		}
		if filter.Search != "" && !strings.Contains(searchable, filter.Search) {
			continue
		}
		row.DebugAvailable = row.DebugAvailable && debugLogExists(row.ID)
		matches = append(matches, row)
		stats.Requests++
		stats.Input += int64(row.InputTokens)
		stats.Output += int64(row.OutputTokens)
		total := row.TotalTokens
		if total == 0 {
			total = row.InputTokens + row.OutputTokens
		}
		stats.Total += int64(total)
		if row.CachedTokens != nil {
			stats.Cached += int64(*row.CachedTokens)
			cacheInput += int64(row.InputTokens)
			stats.CacheSamples++
		}
		if row.Status == "ok" && row.Stream && row.FirstTokenMS != nil && row.DurationMS > *row.FirstTokenMS && row.OutputTokens > 0 {
			speedOutput += int64(row.OutputTokens)
			generationMS += row.DurationMS - *row.FirstTokenMS
			stats.SpeedSamples++
		}
	}
	if cacheInput > 0 {
		rate := float64(stats.Cached) / float64(cacheInput) * 100
		stats.CacheRate = &rate
	}
	if generationMS > 0 {
		rate := float64(speedOutput) * 1000 / float64(generationMS)
		stats.TokenRate = &rate
	}
	pages := max(1, (len(matches)+filter.PageSize-1)/filter.PageSize)
	page := min(max(1, filter.Page), pages)
	start := min((page-1)*filter.PageSize, len(matches))
	end := min(start+filter.PageSize, len(matches))
	return map[string]any{"logs": matches[start:end], "stats": stats, "page": page, "pages": pages, "total": len(matches), "retained": len(logs), "retention_limit": maxRequestLogs, "models": models, "keys": keys}
}
