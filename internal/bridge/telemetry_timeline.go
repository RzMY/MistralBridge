package bridge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"
	_ "time/tzdata" // CPA containers and Windows need not ship a zoneinfo tree.
)

const timelineDays = 90

func bucketIdentity(stamp string, row RequestLog) string {
	raw, _ := json.Marshal([]string{stamp, row.Model, row.UpstreamModel, row.KeyID, row.Thinking, row.Status})
	return string(raw)
}

func addMinute(buckets map[string]*usageBucket, row RequestLog) {
	if row.CreatedAt.IsZero() || row.CreatedAt.Before(time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -timelineDays)) {
		return
	}
	stamp := row.CreatedAt.UTC().Truncate(time.Minute).Format(time.RFC3339)
	key := bucketIdentity(stamp, row)
	b := buckets[key]
	if b == nil {
		b = &usageBucket{Day: stamp, Model: row.Model, UpstreamModel: row.UpstreamModel, KeyID: row.KeyID, KeyLabel: row.KeyLabel, Thinking: row.Thinking, Status: row.Status}
		buckets[key] = b
	}
	b.add(totalsFor(row))
	if row.CreatedAt.After(b.LastSeen) {
		b.LastSeen = row.CreatedAt
		b.KeyLabel = row.KeyLabel
	}
}

// Older writers can drop optional minute buckets without losing daily totals.
// Rebuild only from unambiguous retained records, never add them to daily totals.
func (s *telemetryStore) bootstrapMinutes(rows []RequestLog) {
	if s.data.Minutes != nil {
		return
	}
	s.data.Minutes = map[string]*usageBucket{}
	candidates := map[string][]RequestLog{}
	seen := map[string]bool{}
	legacyImport := false
	for _, reason := range s.data.Reasons {
		if reason == "history_before_upgrade_unknown" {
			legacyImport = true
		}
		if reason == "unverified_legacy_records" {
			return
		}
	}
	for _, row := range rows {
		if seen[row.ID] || !s.data.Known[row.ID] || row.CreatedAt.IsZero() {
			continue
		}
		seen[row.ID] = true
		key := bucketIdentity(row.CreatedAt.UTC().Format("2006-01-02"), row)
		candidates[key] = append(candidates[key], row)
	}
	for key, group := range candidates {
		b := s.data.Buckets[key]
		if b == nil {
			continue
		}
		var sum usageTotals
		for _, row := range group {
			sum.add(totalsFor(row))
		}
		exceeds := func() bool {
			return sum.Requests > b.Requests || sum.Total > b.Total || sum.Input > b.Input || sum.Output > b.Output
		}
		if exceeds() && !legacyImport {
			// A previous writer may have reset the epoch while retaining old
			// detail rows. First-request start times alone are NOT a reset marker.
			kept := make([]RequestLog, 0, len(group))
			sum = usageTotals{}
			for _, row := range group {
				if !row.CreatedAt.Before(s.data.StartedAt) {
					kept = append(kept, row)
					sum.add(totalsFor(row))
				}
			}
			group = kept
		}
		if exceeds() {
			continue
		}
		for _, row := range group {
			addMinute(s.data.Minutes, row)
		}
	}
}

func (s *telemetryStore) pruneMinutes() {
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -timelineDays).Format(time.RFC3339)
	for key, bucket := range s.data.Minutes {
		if bucket.Day < cutoff {
			delete(s.data.Minutes, key)
		}
	}
}

// Old snapshots have no first-token sums. Backfill only when retained records
// account for the whole bucket, so reset epochs or missing rows cannot invent
// an average. Existing samples are never overwritten or counted twice.
func (s *telemetryStore) backfillFirstToken(rows []RequestLog) {
	for _, reason := range s.data.Reasons {
		if reason == "unverified_legacy_records" {
			return
		}
	}
	daily, minutes := map[string]usageTotals{}, map[string]usageTotals{}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] || !s.data.Known[row.ID] {
			continue
		}
		seen[row.ID] = true
		day := row.CreatedAt.UTC().Format("2006-01-02")
		if row.CreatedAt.IsZero() {
			day = "unknown"
		}
		value := totalsFor(row)
		key := bucketIdentity(day, row)
		v := daily[key]
		v.add(value)
		daily[key] = v
		if !row.CreatedAt.IsZero() {
			key = bucketIdentity(row.CreatedAt.UTC().Truncate(time.Minute).Format(time.RFC3339), row)
			v = minutes[key]
			v.add(value)
			minutes[key] = v
		}
	}
	fill := func(buckets map[string]*usageBucket, source map[string]usageTotals) {
		for key, b := range buckets {
			v := source[key]
			if b.FirstTokenTracked == 0 && v.Requests > 0 && v.Requests == b.Requests && v.Total == b.Total && v.DurationMS == b.DurationMS {
				b.FirstTokenMS, b.FirstTokenSamples, b.FirstTokenTracked = v.FirstTokenMS, v.FirstTokenSamples, v.FirstTokenTracked
			}
		}
	}
	fill(s.data.Buckets, daily)
	fill(s.data.Minutes, minutes)
}

func matchUsageBucket(b *usageBucket, q url.Values) bool {
	for _, pair := range [][2]string{{"model", b.Model}, {"upstream_model", b.UpstreamModel}, {"key_id", b.KeyID}, {"thinking", b.Thinking}, {"status", b.Status}} {
		if value := q.Get(pair[0]); value != "" && value != pair[1] {
			return false
		}
	}
	return true
}

func subtractUsage(a, b usageTotals) usageTotals {
	return usageTotals{Requests: a.Requests - b.Requests, Success: a.Success - b.Success, Errors: a.Errors - b.Errors, Input: a.Input - b.Input, Output: a.Output - b.Output, Total: a.Total - b.Total, Cached: a.Cached - b.Cached, CacheInput: a.CacheInput - b.CacheInput, CacheSamples: a.CacheSamples - b.CacheSamples, SpeedOutput: a.SpeedOutput - b.SpeedOutput, GenerationMS: a.GenerationMS - b.GenerationMS, SpeedSamples: a.SpeedSamples - b.SpeedSamples, DurationMS: a.DurationMS - b.DurationMS, UnknownUsage: a.UnknownUsage - b.UnknownUsage, FirstTokenMS: a.FirstTokenMS - b.FirstTokenMS, FirstTokenSamples: a.FirstTokenSamples - b.FirstTokenSamples, FirstTokenTracked: a.FirstTokenTracked - b.FirstTokenTracked}
}

type timelinePoint struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Label string    `json:"label"`
	usageView
}

func queryUsageTimeline(q url.Values) ([]byte, error) {
	bad := func(message string) ([]byte, error) {
		return managementJSONStatus(400, map[string]string{"error": message})
	}
	for key := range q {
		switch key {
		case "from", "to", "timezone", "granularity", "model", "upstream_model", "key_id", "thinking", "status":
		default:
			return bad("不支持的统计参数：" + key)
		}
	}
	zone := q.Get("timezone")
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return bad("无效时区")
	}
	var from, to time.Time
	if q.Get("from") != "" || q.Get("to") != "" {
		from, err = time.Parse(time.RFC3339, q.Get("from"))
		if err != nil {
			return bad("起始时间无效")
		}
		to, err = time.Parse(time.RFC3339, q.Get("to"))
		if err != nil || !to.After(from) {
			return bad("结束时间必须晚于起始时间")
		}
		if from.Second() != 0 || from.Nanosecond() != 0 || to.Second() != 0 || to.Nanosecond() != 0 {
			return bad("统计时间范围必须按分钟对齐")
		}
		if to.Sub(from) > time.Hour*24*366*100 {
			return bad("查询范围过大")
		}
	}
	grain := q.Get("granularity")
	if grain == "" || grain == "auto" {
		grain = "day"
		if !from.IsZero() && (to.Sub(from) <= 24*time.Hour || from.In(loc).Format("2006-01-02") == to.Add(-time.Nanosecond).In(loc).Format("2006-01-02")) {
			grain = "hour"
		}
	}
	if grain != "hour" && grain != "day" {
		return bad("不支持的统计粒度")
	}
	if grain == "hour" && (from.IsZero() || to.Sub(from) > 26*time.Hour) {
		return bad("分时查询最多支持一个自然日")
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
	points := map[string]*timelinePoint{}
	var order []string
	addPoint := func(start, end time.Time, label string) *timelinePoint {
		key := start.UTC().Format(time.RFC3339)
		if p := points[key]; p != nil {
			return p
		}
		p := &timelinePoint{Start: start, End: end, Label: label}
		points[key] = p
		order = append(order, key)
		return p
	}
	// Local-hour boundaries, including half-hour zones and 23/25-hour DST days.
	if grain == "hour" {
		for cursor := from; cursor.Before(to); {
			local := cursor.In(loc)
			next := cursor.Add(time.Hour - time.Duration(local.Minute())*time.Minute)
			if next.After(to) {
				next = to
			}
			addPoint(cursor, next, local.Format("01-02 15:04"))
			cursor = next
		}
	}
	knownDaily := map[string]usageTotals{}
	var total usageTotals
	for _, b := range s.data.Minutes {
		if !matchUsageBucket(b, q) {
			continue
		}
		stamp, err := time.Parse(time.RFC3339, b.Day)
		if err != nil {
			continue
		}
		key := bucketIdentity(stamp.UTC().Format("2006-01-02"), RequestLog{Model: b.Model, UpstreamModel: b.UpstreamModel, KeyID: b.KeyID, Thinking: b.Thinking, Status: b.Status})
		v := knownDaily[key]
		v.add(b.usageTotals)
		knownDaily[key] = v
		if !from.IsZero() && (stamp.Before(from) || !stamp.Before(to)) {
			continue
		}
		total.add(b.usageTotals)
		if grain == "hour" {
			for _, key := range order {
				p := points[key]
				if !stamp.Before(p.Start) && stamp.Before(p.End) {
					p.usageTotals.add(b.usageTotals)
					break
				}
			}
		} else {
			local := stamp.In(loc)
			start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
			addPoint(start, start.AddDate(0, 0, 1), start.Format("2006-01-02")).usageTotals.add(b.usageTotals)
		}
	}
	models, upstreams := map[string]bool{}, map[string]bool{}
	keys := map[string]string{}
	partial := false
	var undistributed usageTotals
	for key, b := range s.data.Buckets {
		models[b.Model] = true
		upstreams[b.UpstreamModel] = true
		if b.KeyID != "" {
			keys[b.KeyID] = b.KeyLabel
		}
		if !matchUsageBucket(b, q) {
			continue
		}
		missing := subtractUsage(b.usageTotals, knownDaily[key])
		if missing.Requests <= 0 {
			continue
		}
		day, e := time.Parse("2006-01-02", b.Day)
		if from.IsZero() {
			total.add(missing)
			undistributed.add(missing)
			partial = true
			continue
		}
		if e != nil || !day.Before(to) || !day.AddDate(0, 0, 1).After(from) {
			continue
		}
		partial = true
		// Entire UTC days may safely contribute to totals, but never invent an
		// hourly or local-date allocation for their missing minute data.
		if !day.Before(from) && !day.AddDate(0, 0, 1).After(to) {
			total.add(missing)
			undistributed.add(missing)
		}
	}
	series := make([]timelinePoint, 0, len(points))
	for _, p := range points {
		p.usageView = viewUsage(p.usageTotals)
		series = append(series, *p)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Start.Before(series[j].Start) })
	stateMu.RLock()
	for _, key := range state.Keys {
		if _, ok := keys[key.ID]; ok {
			keys[key.ID] = key.Label
		}
	}
	stateMu.RUnlock()
	return managementJSON(map[string]any{"stats": viewUsage(total), "series": series, "undistributed": viewUsage(undistributed), "granularity": grain, "timezone": zone, "from": from, "to": to, "timeline_partial": partial, "timeline_retention_days": timelineDays, "server_time": time.Now().UTC(), "started_at": s.data.StartedAt, "epoch": s.data.Epoch, "incomplete": len(s.data.Reasons) > 0, "incomplete_reasons": s.data.Reasons, "models": models, "upstream_models": upstreams, "keys": keys, "clock_source": fmt.Sprintf("request timestamps: server clock; calendar: %s", zone)})
}
