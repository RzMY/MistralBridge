package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const telemetryVersion = 1
const telemetryJournalLimit = 8 << 20
const telemetryRecordLimit = 1 << 20

type retentionConfig struct {
	Version    int   `json:"version"`
	MaxEntries int   `json:"max_entries"`
	MaxDays    int   `json:"max_days"`
	MaxBytes   int64 `json:"max_bytes"`
}

func defaultRetention() retentionConfig {
	return retentionConfig{telemetryVersion, maxRequestLogs, 30, 32 << 20}
}

func (c retentionConfig) validate() error {
	if c.Version != telemetryVersion || c.MaxEntries < 1 || c.MaxEntries > maxRequestLogs || c.MaxDays < 1 || c.MaxDays > 3650 || c.MaxBytes < 1<<20 || c.MaxBytes > 256<<20 {
		return errors.New("日志保留设置无效：条数 1–10000，天数 1–3650，容量 1–256 MiB")
	}
	return nil
}

// Keep additive quantities, never averages of averages. These fields retain the
// existing logStats sample eligibility rules, including unknown legacy usage.
type usageTotals struct {
	Requests          int64 `json:"requests"`
	Success           int64 `json:"success"`
	Errors            int64 `json:"errors"`
	Input             int64 `json:"input_tokens"`
	Output            int64 `json:"output_tokens"`
	Total             int64 `json:"total_tokens"`
	Cached            int64 `json:"cached_tokens"`
	CacheInput        int64 `json:"cache_input_tokens"`
	CacheSamples      int64 `json:"cache_samples"`
	SpeedOutput       int64 `json:"speed_output_tokens"`
	GenerationMS      int64 `json:"generation_ms"`
	SpeedSamples      int64 `json:"speed_samples"`
	DurationMS        int64 `json:"duration_ms"`
	UnknownUsage      int64 `json:"unknown_usage"`
	FirstTokenMS      int64 `json:"first_token_ms_sum"`
	FirstTokenSamples int64 `json:"first_token_samples"`
	FirstTokenTracked int64 `json:"first_token_tracked_requests"`
}

func totalsFor(row RequestLog) usageTotals {
	t := usageTotals{Requests: 1, Input: int64(row.InputTokens), Output: int64(row.OutputTokens), Total: int64(row.TotalTokens), DurationMS: row.DurationMS, FirstTokenTracked: 1}
	if row.Status == "ok" {
		t.Success = 1
	} else {
		t.Errors = 1
	}
	if t.Total == 0 {
		t.Total = t.Input + t.Output
	}
	if !row.UsageKnown && t.Total == 0 && row.CachedTokens == nil {
		t.UnknownUsage = 1
	}
	if row.CachedTokens != nil {
		t.Cached, t.CacheInput, t.CacheSamples = int64(*row.CachedTokens), t.Input, 1
	}
	if row.Status == "ok" && row.Stream && row.FirstTokenMS != nil && *row.FirstTokenMS >= 0 {
		t.FirstTokenMS, t.FirstTokenSamples = *row.FirstTokenMS, 1
	}
	if row.Status == "ok" && row.Stream && row.FirstTokenMS != nil && row.DurationMS > *row.FirstTokenMS && row.OutputTokens > 0 {
		t.SpeedOutput, t.GenerationMS, t.SpeedSamples = t.Output, row.DurationMS-*row.FirstTokenMS, 1
	}
	return t
}

func (t *usageTotals) add(v usageTotals) {
	t.Requests += v.Requests
	t.Success += v.Success
	t.Errors += v.Errors
	t.Input += v.Input
	t.Output += v.Output
	t.Total += v.Total
	t.Cached += v.Cached
	t.CacheInput += v.CacheInput
	t.CacheSamples += v.CacheSamples
	t.SpeedOutput += v.SpeedOutput
	t.GenerationMS += v.GenerationMS
	t.SpeedSamples += v.SpeedSamples
	t.DurationMS += v.DurationMS
	t.UnknownUsage += v.UnknownUsage
	t.FirstTokenMS += v.FirstTokenMS
	t.FirstTokenSamples += v.FirstTokenSamples
	t.FirstTokenTracked += v.FirstTokenTracked
}

type usageBucket struct {
	Day           string    `json:"day"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model"`
	KeyID         string    `json:"key_id"`
	KeyLabel      string    `json:"key_label"`
	Thinking      string    `json:"thinking"`
	Status        string    `json:"status"`
	LastSeen      time.Time `json:"last_seen"`
	usageTotals
}

type telemetrySnapshot struct {
	Version   int                     `json:"version"`
	Sequence  uint64                  `json:"sequence"`
	Epoch     string                  `json:"epoch"`
	StartedAt time.Time               `json:"started_at"`
	Reasons   []string                `json:"incomplete_reasons"`
	Buckets   map[string]*usageBucket `json:"buckets"`
	Minutes   map[string]*usageBucket `json:"minutes"`
	// Only IDs still in the compatibility file (plus uncheckpointed events).
	Known      map[string]bool `json:"known_ids"`
	LegacyTail string          `json:"legacy_tail,omitempty"`
	ClearLogs  bool            `json:"clear_logs,omitempty"`
}

type telemetryEvent struct {
	Version  int        `json:"version"`
	Sequence uint64     `json:"sequence"`
	Entry    RequestLog `json:"entry"`
}

type telemetryStore struct {
	path           string
	config         retentionConfig
	data           telemetrySnapshot
	journalBytes   int64
	uncheckpointed int
	legacyBytes    int64
	oldest         time.Time
}

// All store access, migration, pruning and clear/reset operations use logMu.
var telemetry *telemetryStore
var telemetryFailurePath string

func (s *telemetryStore) file(name string) string {
	return filepath.Join(filepath.Dir(s.path), "telemetry", name)
}

func (s *telemetryStore) incomplete(reason string) {
	for _, existing := range s.data.Reasons {
		if existing == reason {
			return
		}
	}
	s.data.Reasons = append(s.data.Reasons, reason)
}

func (s *telemetryStore) include(row RequestLog) {
	day := row.CreatedAt.UTC().Format("2006-01-02")
	if row.CreatedAt.IsZero() {
		day = "unknown"
		s.incomplete("missing_request_time")
	}
	identity, _ := json.Marshal([]string{day, row.Model, row.UpstreamModel, row.KeyID, row.Thinking, row.Status})
	key := string(identity)
	bucket := s.data.Buckets[key]
	if bucket == nil {
		bucket = &usageBucket{Day: day, Model: row.Model, UpstreamModel: row.UpstreamModel, KeyID: row.KeyID, Thinking: row.Thinking, Status: row.Status}
		s.data.Buckets[key] = bucket
	}
	if !row.CreatedAt.Before(bucket.LastSeen) {
		bucket.KeyLabel = row.KeyLabel
		bucket.LastSeen = row.CreatedAt
	}
	bucket.add(totalsFor(row))
	s.data.Known[row.ID] = true
	if s.data.Minutes != nil {
		addMinute(s.data.Minutes, row)
	}
}

// atomicTelemetryFile synchronizes file contents before publishing the new
// generation. The old generation is never truncated in place.
func atomicTelemetryFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".telemetry-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// Unix also needs the directory entry synchronized after replacement.
	// Windows does not support syncing a directory handle through os.File.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

func (s *telemetryStore) checkpoint() error {
	s.pruneMinutes()
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	if err = atomicTelemetryFile(s.file("snapshot.json"), raw); err != nil {
		return err
	}
	// A crash before this replacement leaves old events, skipped by Sequence.
	if err = atomicTelemetryFile(s.file("journal.jsonl"), nil); err != nil {
		return err
	}
	s.journalBytes, s.uncheckpointed = 0, 0
	return nil
}

// scanCompatibility reads all on-disk rows, not just readRequestLogs' last
// 10000. Invalid complete rows are reported as a coverage gap, not silently
// treated as a complete import. A partial tail can be discarded after import.
func scanCompatibility(path string) ([]RequestLog, bool, error) {
	f, err := os.Open(logPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), telemetryRecordLimit)
	var rows []RequestLog
	invalid := false
	for scanner.Scan() {
		var row RequestLog
		if json.Unmarshal(scanner.Bytes(), &row) != nil || row.ID == "" {
			invalid = true
			continue
		}
		rows = append(rows, row)
	}
	return rows, invalid, scanner.Err()
}

func (s *telemetryStore) replay() ([]RequestLog, error) {
	raw, err := os.ReadFile(s.file("journal.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > telemetryJournalLimit {
		return nil, errors.New("统计 journal 超过恢复上限")
	}
	// Only an incomplete last record is recoverable automatically.
	end := bytes.LastIndexByte(raw, '\n') + 1
	if end != len(raw) {
		s.incomplete("interrupted_journal_write")
		// Defer repair until the complete records have all been validated and
		// checkpointed. An unknown future format must remain untouched.
		raw = raw[:end]
	}
	s.journalBytes = int64(len(raw))
	var pending []RequestLog
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event telemetryEvent
		if json.Unmarshal(line, &event) != nil || event.Version != telemetryVersion || event.Entry.ID == "" {
			return nil, errors.New("统计 journal 格式不兼容或已损坏，原文件已保留")
		}
		if event.Sequence <= s.data.Sequence {
			continue
		}
		if event.Sequence != s.data.Sequence+1 {
			return nil, errors.New("统计 journal 序号不连续")
		}
		if !s.data.Known[event.Entry.ID] {
			s.include(event.Entry)
		}
		s.data.Sequence = event.Sequence
		pending = append(pending, event.Entry)
	}
	return pending, nil
}

func ensureTelemetryLocked(path string) (*telemetryStore, error) {
	if telemetry != nil && telemetry.path == path {
		return telemetry, nil
	}
	s := &telemetryStore{path: path, config: defaultRetention(), data: telemetrySnapshot{Version: telemetryVersion, Epoch: randomID(), StartedAt: time.Now().UTC(), Buckets: map[string]*usageBucket{}, Known: map[string]bool{}}}
	config, err := os.ReadFile(s.file("config.json"))
	if err == nil {
		if json.Unmarshal(config, &s.config) != nil {
			return nil, errors.New("统计设置已损坏，原文件已保留")
		}
		if err := s.config.validate(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw, err := os.ReadFile(s.file("snapshot.json"))
	existed := err == nil
	if existed {
		if json.Unmarshal(raw, &s.data) != nil || s.data.Version != telemetryVersion || s.data.Buckets == nil || s.data.Known == nil || s.data.Epoch == "" {
			return nil, errors.New("统计快照格式不兼容或已损坏，原文件已保留")
		}
		for _, b := range s.data.Minutes {
			if b == nil {
				return nil, errors.New("统计快照包含无效分时汇总")
			}
		}
		for _, b := range s.data.Buckets {
			if b == nil {
				return nil, errors.New("统计快照包含无效汇总")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	pending, err := s.replay()
	if err != nil {
		return nil, err
	}
	if s.data.ClearLogs {
		if err := atomicTelemetryFile(logPath(path), nil); err != nil {
			return nil, err
		}
		s.data.Known = map[string]bool{}
		s.data.LegacyTail = ""
		s.data.ClearLogs = false
	}
	rows, invalid, err := scanCompatibility(path)
	if err != nil {
		return nil, err
	}
	if invalid {
		s.incomplete("invalid_legacy_records")
	}
	if !existed && len(rows) > 0 {
		s.incomplete("history_before_upgrade_unknown")
	}
	onDisk := make(map[string]bool, len(rows))
	for _, row := range rows {
		onDisk[row.ID] = true
	}
	// Journal events are authoritative. Restore missing compatibility records
	// before committing a checkpoint that allows their journal to be recycled.
	for _, row := range pending {
		if !onDisk[row.ID] {
			if err := appendCompatibility(path, row); err != nil {
				return nil, err
			}
			rows = append(rows, row)
			onDisk[row.ID] = true
		}
	}
	for id := range s.data.Known {
		if !onDisk[id] {
			s.incomplete("legacy_history_may_be_missing")
			break
		}
	}
	// Only an append following the last projected record establishes a safe
	// import boundary. With no surviving anchor, a rotated file and a restored
	// old backup cannot be distinguished without an unbounded lifetime ID set.
	boundary := -1
	if existed && s.data.LegacyTail != "" {
		for i, row := range rows {
			if row.ID == s.data.LegacyTail {
				boundary = i
			}
		}
	}
	for i, row := range rows {
		if !s.data.Known[row.ID] {
			if existed {
				s.incomplete("legacy_history_may_be_missing")
			}
			if existed && s.data.LegacyTail != "" && (boundary < 0 || i <= boundary) {
				s.incomplete("unverified_legacy_records")
				s.data.Known[row.ID] = true // excluded, do not reimport on next restart
				continue
			}
			s.include(row)
		}
	}
	if len(rows) > 0 {
		s.data.LegacyTail = rows[len(rows)-1].ID
	} else {
		s.data.LegacyTail = ""
	}
	if telemetryFailurePath == path {
		s.incomplete("storage_write_failed")
	}
	s.bootstrapMinutes(rows)
	s.backfillFirstToken(rows)
	// Preserve imported totals BEFORE rewriting or limiting the source file.
	if err := s.checkpoint(); err != nil {
		return nil, err
	}
	if err := s.retain(rows, true); err != nil {
		return nil, err
	}
	telemetry = s
	telemetryFailurePath = ""
	return s, nil
}

func appendCompatibility(path string, row RequestLog) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendSynced(logPath(path), append(raw, '\n'), true)
}

func appendSynced(path string, raw []byte, alignJSONL ...bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	start, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if start > 0 && len(alignJSONL) > 0 && alignJSONL[0] {
		var last [1]byte
		if _, err := f.ReadAt(last[:], start-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			raw = append([]byte{'\n'}, raw...)
		}
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Truncate(start)
		_ = f.Sync()
		return err
	}
	return f.Close()
}

func retainedRows(rows []RequestLog, c retentionConfig, now time.Time) ([]RequestLog, []byte) {
	cutoff := now.Add(-time.Duration(c.MaxDays) * 24 * time.Hour)
	selected := make([]RequestLog, 0, min(len(rows), c.MaxEntries))
	lines := make([][]byte, 0, cap(selected))
	seen := map[string]bool{}
	var size int64
	for i := len(rows) - 1; i >= 0 && len(selected) < c.MaxEntries; i-- {
		row := rows[i]
		if seen[row.ID] || row.CreatedAt.Before(cutoff) {
			continue
		}
		seen[row.ID] = true
		raw, err := json.Marshal(row)
		if err != nil || int64(len(raw)+1) > c.MaxBytes {
			continue
		}
		if size+int64(len(raw)+1) > c.MaxBytes {
			break
		}
		size += int64(len(raw) + 1)
		selected = append(selected, row)
		lines = append(lines, raw)
	}
	out := make([]RequestLog, 0, len(selected))
	raw := make([]byte, 0, size)
	for i := len(selected) - 1; i >= 0; i-- {
		out = append(out, selected[i])
		raw = append(raw, lines[i]...)
		raw = append(raw, '\n')
	}
	return out, raw
}

func (s *telemetryStore) retain(rows []RequestLog, force bool) error {
	now := time.Now()
	if !force && len(rows) <= s.config.MaxEntries && s.legacyBytes <= s.config.MaxBytes && (len(rows) == 0 || !s.oldest.Before(now.Add(-time.Duration(s.config.MaxDays)*24*time.Hour))) {
		stateMu.Lock()
		state.Logs = rows
		stateMu.Unlock()
		return nil
	}
	config := s.config
	// Reclaim headroom in batches: a single legacy JSONL file otherwise needs
	// to be rewritten for every request after reaching its limit.
	if len(rows) > config.MaxEntries {
		config.MaxEntries = max(1, config.MaxEntries*9/10)
	}
	if s.legacyBytes > config.MaxBytes {
		config.MaxBytes = config.MaxBytes * 9 / 10
	}
	kept, raw := retainedRows(rows, config, now)
	if force || len(kept) != len(rows) {
		// All rows must already be projected; checkpoint before removing source.
		if err := s.checkpoint(); err != nil {
			return err
		}
		if err := atomicTelemetryFile(logPath(s.path), raw); err != nil {
			return err
		}
		s.data.Known = make(map[string]bool, len(kept))
		for _, row := range kept {
			s.data.Known[row.ID] = true
		}
		s.data.LegacyTail = ""
		if len(kept) > 0 {
			s.data.LegacyTail = kept[len(kept)-1].ID
		}
		if err := s.checkpoint(); err != nil {
			return err
		}
	}
	s.legacyBytes = int64(len(raw))
	s.oldest = time.Time{}
	for _, row := range kept {
		if s.oldest.IsZero() || row.CreatedAt.Before(s.oldest) {
			s.oldest = row.CreatedAt
		}
	}
	stateMu.Lock()
	state.Logs = kept
	stateMu.Unlock()
	return nil
}

func (s *telemetryStore) record(row RequestLog) error {
	if s.data.Known[row.ID] {
		return nil
	}
	event := telemetryEvent{Version: telemetryVersion, Sequence: s.data.Sequence + 1, Entry: row}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(raw) >= telemetryRecordLimit {
		return errors.New("请求元数据超过统计单条上限")
	}
	if s.journalBytes+int64(len(raw)+1) > telemetryJournalLimit {
		if err := s.checkpoint(); err != nil {
			return err
		}
	}
	if err := appendSynced(s.file("journal.jsonl"), append(raw, '\n')); err != nil {
		return err
	}
	s.journalBytes += int64(len(raw) + 1)
	s.include(row)
	s.data.Sequence = event.Sequence
	s.uncheckpointed++
	if err := appendCompatibility(s.path, row); err != nil {
		return err
	}
	s.data.LegacyTail = row.ID
	line, _ := json.Marshal(row)
	s.legacyBytes += int64(len(line) + 1)
	if s.oldest.IsZero() || row.CreatedAt.Before(s.oldest) {
		s.oldest = row.CreatedAt
	}
	stateMu.Lock()
	rows := append(state.Logs, row)
	state.Logs = rows
	stateMu.Unlock()
	if err := s.retain(rows, false); err != nil {
		return err
	}
	if s.uncheckpointed >= 128 {
		return s.checkpoint()
	}
	return nil
}

func telemetryErrorLocked(path string, err error) error {
	telemetry = nil // retry from durable state; never checkpoint unprojected data
	telemetryFailurePath = path
	return fmt.Errorf("统计存储不可用：%w", err)
}

func closeTelemetry() {
	logMu.Lock()
	defer logMu.Unlock()
	if telemetry != nil {
		if err := telemetry.checkpoint(); err != nil {
			_ = telemetryErrorLocked(telemetry.path, err)
		}
	}
}
