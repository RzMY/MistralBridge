package bridge

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type timelineTestResult struct {
	Stats   usageView       `json:"stats"`
	Series  []timelinePoint `json:"series"`
	Partial bool            `json:"timeline_partial"`
	Grain   string          `json:"granularity"`
	Zone    string          `json:"timezone"`
}

func TestTimelinePerPeriodMetricsAndFirstTokenMigration(t *testing.T) {
	isolatedState(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	for i, first := range []int64{100, 300} {
		cache := 4
		row := usageRow(string(rune('a' + i)))
		row.CreatedAt = now.Add(time.Duration(i) * time.Minute)
		row.Stream, row.FirstTokenMS, row.CachedTokens = true, &first, &cache
		row.DurationMS = first + 1000
		if err := appendRequestLog(row); err != nil {
			t.Fatal(err)
		}
	}
	plain := usageRow("plain")
	plain.CreatedAt = now.Add(2 * time.Minute)
	plain.DurationMS = 600
	if err := appendRequestLog(plain); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"timezone": {"Asia/Singapore"}, "from": {now.Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}
	verify := func() {
		t.Helper()
		r := timelineResult(t, query)
		if len(r.Series) != 1 {
			t.Fatalf("series=%+v", r.Series)
		}
		v := r.Series[0]
		if v.CacheRate == nil || *v.CacheRate != 40 || v.TokenRate == nil || *v.TokenRate != 5 || v.AverageFirstTokenMS == nil || *v.AverageFirstTokenMS != 200 || v.AverageMS == nil || *v.AverageMS != 1000 {
			t.Fatalf("weighted metrics: %+v", v)
		}
		if strings.Contains(v.Label, "+08") || strings.Contains(v.Label, "UTC") {
			t.Fatal("time label exposes offset")
		}
	}
	verify()
	closeTelemetry()
	// A deployed test.3 snapshot has minute buckets but no first-token sums.
	file := telemetry.file("snapshot.json")
	raw, _ := os.ReadFile(file)
	var legacy map[string]any
	_ = json.Unmarshal(raw, &legacy)
	for _, name := range []string{"buckets", "minutes"} {
		for _, item := range legacy[name].(map[string]any) {
			bucket := item.(map[string]any)
			delete(bucket, "first_token_ms_sum")
			delete(bucket, "first_token_samples")
			delete(bucket, "first_token_tracked_requests")
		}
	}
	raw, _ = json.Marshal(legacy)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		restartTelemetry()
		verify()
	}
	if err := clearRequestLogs(); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	verify()
	if v := viewUsage(totalsFor(plain)); v.AverageFirstTokenMS != nil {
		t.Fatal("non-stream request invented first-token average")
	}
}

func timelineResult(t *testing.T, q url.Values) timelineTestResult {
	t.Helper()
	raw, err := queryHistoricalStats(q)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	var response managementResponse
	var result timelineTestResult
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &response) != nil || response.StatusCode != 200 || json.Unmarshal(response.Body, &result) != nil {
		t.Fatalf("response %s", raw)
	}
	return result
}

func TestTimelineLocalDayAndMinuteBoundaries(t *testing.T) {
	isolatedState(t)
	loc, _ := time.LoadLocation("Asia/Singapore")
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	for i, offset := range []time.Duration{-time.Minute, 30 * time.Minute, 90 * time.Minute, 24 * time.Hour} {
		row := usageRow(string(rune('a' + i)))
		row.CreatedAt = start.Add(offset)
		if err := appendRequestLog(row); err != nil {
			t.Fatal(err)
		}
	}
	q := url.Values{"timezone": {"Asia/Singapore"}, "from": {start.Format(time.RFC3339)}, "to": {start.AddDate(0, 0, 1).Format(time.RFC3339)}}
	r := timelineResult(t, q)
	if r.Grain != "hour" || r.Zone != "Asia/Singapore" || len(r.Series) != 24 || r.Stats.Requests != 2 || r.Series[0].Total != 15 || r.Series[1].Total != 15 || r.Partial {
		t.Fatalf("local-day result %+v", r)
	}
	q.Set("from", start.Add(31*time.Minute).Format(time.RFC3339))
	q.Set("to", start.Add(91*time.Minute).Format(time.RFC3339))
	r = timelineResult(t, q)
	if r.Stats.Requests != 1 || r.Stats.Total != 15 || r.Grain != "hour" {
		t.Fatalf("partial range %+v", r)
	}
	if err := clearRequestLogs(); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	r = timelineResult(t, q)
	if r.Stats.Requests != 1 {
		t.Fatal("clearing details lost durable timeline")
	}
}

func TestTimelineRebuildsOldSnapshotWithoutDoubleCounting(t *testing.T) {
	isolatedState(t)
	for _, id := range []string{"one", "two"} {
		if err := appendRequestLog(usageRow(id)); err != nil {
			t.Fatal(err)
		}
	}
	closeTelemetry()
	file := telemetry.file("snapshot.json")
	raw, _ := os.ReadFile(file)
	var old map[string]json.RawMessage
	_ = json.Unmarshal(raw, &old)
	delete(old, "minutes")
	raw, _ = json.Marshal(old)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		restartTelemetry()
		r := timelineResult(t, url.Values{"timezone": {"Asia/Singapore"}})
		if r.Stats.Requests != 2 || len(r.Series) != 1 || r.Series[0].Requests != 2 || r.Partial {
			t.Fatalf("migration %+v", r)
		}
	}
	if response := managementCall(t, "DELETE", "/stats", ""); response.StatusCode != 200 {
		t.Fatal("reset")
	}
	restartTelemetry()
	r := timelineResult(t, url.Values{"timezone": {"Asia/Singapore"}})
	if r.Stats.Requests != 0 || len(r.Series) != 0 {
		t.Fatal("reset reimported timeline")
	}
}

func TestTimelineDSTAndQuarterHourZones(t *testing.T) {
	isolatedState(t)
	for _, c := range []struct {
		zone, from, to string
		hours          int
	}{
		{"America/New_York", "2026-03-08T00:00:00-05:00", "2026-03-09T00:00:00-04:00", 23},
		{"America/New_York", "2026-11-01T00:00:00-04:00", "2026-11-02T00:00:00-05:00", 25},
		{"Asia/Kathmandu", "2026-11-28T00:00:00+05:45", "2026-11-29T00:00:00+05:45", 24},
	} {
		r := timelineResult(t, url.Values{"timezone": {c.zone}, "from": {c.from}, "to": {c.to}})
		if r.Grain != "hour" || len(r.Series) != c.hours {
			t.Fatalf("%s: %s %d", c.zone, r.Grain, len(r.Series))
		}
		for i := 1; i < len(r.Series); i++ {
			if !r.Series[i].Start.Equal(r.Series[i-1].End) {
				t.Fatal("gap/overlap in DST series")
			}
		}
	}
}

func TestTimelineMissingMinuteDataNeverInventsHourlyAllocation(t *testing.T) {
	isolatedState(t)
	if err := appendRequestLog(usageRow("one")); err != nil {
		t.Fatal(err)
	}
	telemetry.data.Minutes = map[string]*usageBucket{}
	all := timelineResult(t, url.Values{"timezone": {"Asia/Singapore"}})
	if all.Stats.Requests != 1 || len(all.Series) != 0 || !all.Partial {
		t.Fatal("daily history lost or fabricated")
	}
	now := time.Now().UTC().Truncate(time.Hour)
	r := timelineResult(t, url.Values{"timezone": {"UTC"}, "from": {now.Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}})
	if r.Stats.Requests != 0 || !r.Partial {
		t.Fatal("unknown sub-day amount reported as complete")
	}
}
