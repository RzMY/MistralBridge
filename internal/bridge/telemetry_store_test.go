package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func usageRow(id string) RequestLog {
	return RequestLog{ID: id, CreatedAt: time.Now().UTC(), Model: "alias", UpstreamModel: "upstream", KeyID: "key", KeyLabel: "Primary", Status: "ok", HTTPStatus: 200, UsageKnown: true, InputTokens: 10, OutputTokens: 5, TotalTokens: 15, DurationMS: 1000}
}

func historyResult(t *testing.T, query url.Values) (usageView, bool) {
	t.Helper()
	raw, err := queryHistoricalStats(query)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	var result managementResponse
	var body struct {
		Stats      usageView `json:"stats"`
		Incomplete bool      `json:"incomplete"`
	}
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &result) != nil || result.StatusCode != 200 || json.Unmarshal(result.Body, &body) != nil {
		t.Fatalf("history: %s", raw)
	}
	return body.Stats, body.Incomplete
}

func restartTelemetry() { logMu.Lock(); telemetry = nil; logMu.Unlock() }

func TestHistoryImportsEntireLegacyFileBeforeRetention(t *testing.T) {
	isolatedState(t)
	path, _ := statePath()
	var content bytes.Buffer
	for i := 0; i < 12050; i++ {
		row := usageRow(fmt.Sprintf("legacy-%d", i))
		if i == 0 {
			row.CreatedAt = time.Now().Add(-90 * 24 * time.Hour)
		}
		_ = json.NewEncoder(&content).Encode(row)
	}
	if err := os.WriteFile(logPath(path), content.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	stats, incomplete := historyResult(t, nil)
	if stats.Requests != 12050 || stats.Total != 12050*15 || !incomplete {
		t.Fatalf("stats=%+v incomplete=%v", stats, incomplete)
	}
	if len(state.Logs) > 10000 || len(state.Logs) == 0 {
		t.Fatal("retention not enforced")
	}
	stored, _ := os.ReadFile(logPath(path))
	if bytes.Count(stored, []byte{'\n'}) != len(state.Logs) {
		t.Fatal("disk and memory differ")
	}
	for range 2 {
		restartTelemetry()
		stats, _ = historyResult(t, nil)
		if stats.Requests != 12050 {
			t.Fatal("migration double counted")
		}
	}
}

func TestHistorySurvivesLegacyRewriteAndReupgrade(t *testing.T) {
	isolatedState(t)
	for _, id := range []string{"one", "two"} {
		if err := appendRequestLog(usageRow(id)); err != nil {
			t.Fatal(err)
		}
	}
	closeTelemetry()
	path, _ := statePath()
	before, err := os.ReadFile(telemetry.file("snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The v0.5.0 writer re-encodes only RequestLog fields and state fields.
	legacy := readRequestLogs(path)
	legacy = append(legacy, usageRow("old-version-three"))
	state.Logs = legacy
	if err := compactRequestLogs(path); err != nil {
		t.Fatal(err)
	}
	if err := mutateState(func(s *pluginState) error { s.DebugMode = true; return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(telemetry.file("snapshot.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("legacy mutation changed sidecar")
	}
	restartTelemetry()
	stats, incomplete := historyResult(t, nil)
	if stats.Requests != 3 || !incomplete {
		t.Fatalf("stats=%+v incomplete=%v", stats, incomplete)
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 3 {
		t.Fatal("old-version events counted twice")
	}
	closeTelemetry()
	// Simulate the old version truncating unaccounted requests before upgrade.
	if err := os.WriteFile(logPath(path), nil, 0600); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, incomplete = historyResult(t, nil)
	if stats.Requests != 3 || !incomplete {
		t.Fatal("lost recorded history or hid a gap")
	}
}

func TestHistoryRecoveryAtJournalAndCheckpointBoundaries(t *testing.T) {
	isolatedState(t)
	historyResult(t, nil)
	path, _ := statePath()
	event := telemetryEvent{Version: telemetryVersion, Sequence: 1, Entry: usageRow("durable")}
	raw, _ := json.Marshal(event)
	raw = append(raw, '\n')
	journal := telemetry.file("journal.jsonl")
	if err := appendSynced(journal, raw); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, incomplete := historyResult(t, nil)
	if stats.Requests != 1 || incomplete || len(state.Logs) != 1 {
		t.Fatalf("WAL replay: %+v incomplete=%v", stats, incomplete)
	}
	stored, _ := os.ReadFile(logPath(path))
	if bytes.Count(stored, []byte{'\n'}) != 1 {
		t.Fatal("missing compatibility projection")
	}
	// Checkpoint published, journal recycling interrupted.
	if err := appendSynced(journal, raw); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 1 {
		t.Fatal("checkpoint replay duplicated event")
	}
	if err := appendSynced(journal, []byte(`{"version":1,"sequence":2`)); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, incomplete = historyResult(t, nil)
	if stats.Requests != 1 || !incomplete {
		t.Fatal("partial journal tail not reported")
	}
}

func TestHistoryClearAndResetDoNotReimportOldRows(t *testing.T) {
	isolatedState(t)
	if err := appendRequestLog(usageRow("one")); err != nil {
		t.Fatal(err)
	}
	if response := managementCall(t, "DELETE", "/stats", ""); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	restartTelemetry()
	stats, _ := historyResult(t, nil)
	if stats.Requests != 0 || len(state.Logs) != 1 {
		t.Fatal("reset reimported old row or deleted details")
	}
	if err := appendRequestLog(usageRow("two")); err != nil {
		t.Fatal(err)
	}
	if err := clearRequestLogs(); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 1 || len(state.Logs) != 0 {
		t.Fatal("clear deleted history or recovered cleared rows")
	}
	if err := appendRequestLog(usageRow("three")); err != nil {
		t.Fatal(err)
	}
	// Crash after persisting a clear intent, before clearing the source file.
	telemetry.data.ClearLogs = true
	if err := telemetry.checkpoint(); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 2 || len(state.Logs) != 0 {
		t.Fatal("interrupted clear was not completed")
	}
}

func TestRetentionLimitsBytesAgeCountAndPersistsSettings(t *testing.T) {
	isolatedState(t)
	response := managementCall(t, "PATCH", "/telemetry/settings", `{"max_entries":3,"max_days":2,"max_bytes":1048576}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	for i := 0; i < 6; i++ {
		row := usageRow(fmt.Sprintf("large-%d", i))
		row.Error = strings.Repeat("x", 400000)
		if err := appendRequestLog(row); err != nil {
			t.Fatal(err)
		}
	}
	old := usageRow("expired")
	old.CreatedAt = time.Now().Add(-3 * 24 * time.Hour)
	if err := appendRequestLog(old); err != nil {
		t.Fatal(err)
	}
	path, _ := statePath()
	info, err := os.Stat(logPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1048576 || len(state.Logs) > 3 {
		t.Fatal("hard limit exceeded")
	}
	for _, row := range state.Logs {
		if row.ID == "expired" {
			t.Fatal("expired row retained")
		}
	}
	stats, _ := historyResult(t, nil)
	if stats.Requests != 7 {
		t.Fatal("retention lost history")
	}
	if len(telemetry.data.Known) > 3 {
		t.Fatal("unbounded ID ledger")
	}
	restartTelemetry()
	historyResult(t, nil)
	if telemetry.config.MaxEntries != 3 || telemetry.config.MaxDays != 2 {
		t.Fatal("settings lost on restart")
	}
	for _, body := range []string{`{"max_entries":0}`, `{"max_bytes":0}`, `{"version":2}`, `{"max_days":-1}`} {
		if r := managementCall(t, "PATCH", "/telemetry/settings", body); r.StatusCode != 400 {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestHistoricalWeightedMetricsAndDateFilters(t *testing.T) {
	isolatedState(t)
	first := int64(100)
	cache := 4
	a := usageRow("a")
	a.Stream = true
	a.FirstTokenMS = &first
	a.CachedTokens = &cache
	a.DurationMS = 1100
	b := usageRow("b")
	b.InputTokens = 90
	b.OutputTokens = 15
	b.TotalTokens = 105
	b.Stream = true
	b.FirstTokenMS = &first
	b.DurationMS = 3100
	b.KeyID = "other"
	b.CreatedAt = a.CreatedAt.Add(-24 * time.Hour)
	c := usageRow("c")
	c.Status = "error"
	c.UsageKnown = false
	c.InputTokens = 0
	c.OutputTokens = 0
	c.TotalTokens = 0
	for _, row := range []RequestLog{a, b, c} {
		if err := appendRequestLog(row); err != nil {
			t.Fatal(err)
		}
	}
	stats, _ := historyResult(t, nil)
	if stats.Requests != 3 || stats.Success != 2 || stats.Errors != 1 || stats.Total != 120 || stats.UnknownUsage != 1 || stats.CacheRate == nil || *stats.CacheRate != 40 || stats.TokenRate == nil || *stats.TokenRate != 5 {
		t.Fatalf("metrics=%+v", stats)
	}
	day := a.CreatedAt.UTC().Format("2006-01-02")
	filtered, _ := historyResult(t, url.Values{"from_day": {day}, "key_id": {"key"}, "status": {"ok"}})
	if filtered.Requests != 1 || filtered.Total != 15 {
		t.Fatalf("filtered=%+v", filtered)
	}
	for _, query := range []url.Values{{"search": {"error"}}, {"from": {"2026-01-01T00:00:00Z"}}, {"from_day": {"bad"}}, {"from_day": {day}, "to_day": {day}}} {
		raw, _ := queryHistoricalStats(query)
		var env envelope
		var reply managementResponse
		_ = json.Unmarshal(raw, &env)
		_ = json.Unmarshal(env.Result, &reply)
		if reply.StatusCode != http.StatusBadRequest {
			t.Fatalf("accepted query %v", query)
		}
	}
}

func TestUnknownTelemetryVersionPreservesFilesAndModelLogs(t *testing.T) {
	isolatedState(t)
	path, _ := statePath()
	file := filepath.Join(filepath.Dir(path), "telemetry", "snapshot.json")
	raw := []byte(`{"version":999,"future_data":"keep"}`)
	if err := atomicTelemetryFile(file, raw); err != nil {
		t.Fatal(err)
	}
	if err := appendRequestLog(usageRow("legacy-fallback")); err == nil {
		t.Fatal("unrecognized version was accepted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(raw, after) {
		t.Fatal("future snapshot was overwritten")
	}
	logs := readRequestLogs(path)
	if len(logs) != 1 {
		t.Fatal("fallback logging stopped")
	}
	if reply := managementCall(t, "GET", "/stats", ""); reply.StatusCode != 503 {
		t.Fatal("unavailable statistics reported healthy")
	}
}

func TestConcurrentHistoryAndJournalBounded(t *testing.T) {
	isolatedState(t)
	var wg sync.WaitGroup
	for i := 0; i < 160; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := appendRequestLog(usageRow(fmt.Sprintf("concurrent-%d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	stats, _ := historyResult(t, nil)
	if stats.Requests != 160 || stats.Total != 2400 {
		t.Fatalf("stats=%+v", stats)
	}
	info, err := os.Stat(telemetry.file("journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= telemetryJournalLimit || telemetry.uncheckpointed >= 128 {
		t.Fatal("journal was not checkpointed")
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 160 {
		t.Fatal("recovery changed concurrent counts")
	}
}

func TestHistoryMissingLegacyAnchorDoesNotRecountRestoredBackup(t *testing.T) {
	isolatedState(t)
	for _, id := range []string{"old", "tail"} {
		if err := appendRequestLog(usageRow(id)); err != nil {
			t.Fatal(err)
		}
	}
	closeTelemetry()
	path, _ := statePath()
	// A restored file lacks the previous tail and includes an unknown row.
	// It could also be an old-version rotation: neither is safe to fully import.
	raw, _ := json.Marshal(usageRow("unverifiable"))
	if err := os.WriteFile(logPath(path), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		restartTelemetry()
		stats, incomplete := historyResult(t, nil)
		if stats.Requests != 2 || !incomplete {
			t.Fatalf("ambiguous history counted: %+v", stats)
		}
	}
}

func TestHistoryRecoversAfterCompatibilityWriteFailure(t *testing.T) {
	isolatedState(t)
	historyResult(t, nil)
	path, _ := statePath()
	if err := os.Rename(logPath(path), logPath(path)+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logPath(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := appendRequestLog(usageRow("journal-only")); err == nil {
		t.Fatal("expected projection failure")
	}
	if err := os.Remove(logPath(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(logPath(path)+".backup", logPath(path)); err != nil {
		t.Fatal(err)
	}
	stats, incomplete := historyResult(t, nil)
	if stats.Requests != 1 || !incomplete || len(state.Logs) != 1 {
		t.Fatalf("lost durable event: %+v", stats)
	}
	restartTelemetry()
	stats, _ = historyResult(t, nil)
	if stats.Requests != 1 {
		t.Fatal("recovery double counted")
	}
}

func TestHistoryCorruptCompleteJournalIsNotSilentlyDiscarded(t *testing.T) {
	isolatedState(t)
	historyResult(t, nil)
	file := telemetry.file("journal.jsonl")
	raw := []byte("{broken}\n")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	if reply := managementCall(t, "GET", "/stats", ""); reply.StatusCode != 503 {
		t.Fatal("corrupt journal accepted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(raw, after) {
		t.Fatal("corrupt journal overwritten")
	}
}

func TestCompatibilityAppendSeparatesInterruptedTail(t *testing.T) {
	isolatedState(t)
	path, _ := statePath()
	if err := os.WriteFile(logPath(path), []byte(`{"id":"unfinished`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := appendCompatibility(path, usageRow("recoverable")); err != nil {
		t.Fatal(err)
	}
	rows, invalid, err := scanCompatibility(path)
	if err != nil || !invalid || len(rows) != 1 || rows[0].ID != "recoverable" {
		t.Fatalf("lost appended row: %+v invalid=%v err=%v", rows, invalid, err)
	}
}

func TestFutureJournalWithPartialTailRemainsUntouched(t *testing.T) {
	isolatedState(t)
	historyResult(t, nil)
	file := telemetry.file("journal.jsonl")
	raw := []byte("{\"version\":999,\"entry\":{\"id\":\"future\"}}\npartial")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restartTelemetry()
	if reply := managementCall(t, "GET", "/stats", ""); reply.StatusCode != 503 {
		t.Fatal("future journal accepted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(raw, after) {
		t.Fatal("future journal partially rewritten")
	}
}
