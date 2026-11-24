package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxDebugLogs      = 100
	maxDebugLogBytes  = 256 * 1024 * 1024
	debugDirectory    = "debug-logs"
	debugRedactedText = "[redacted]"
)

var debugMu sync.RWMutex

type debugExchange struct {
	ID              string        `json:"id"`
	CreatedAt       time.Time     `json:"created_at"`
	DurationMS      int64         `json:"duration_ms"`
	OriginalRequest *debugMessage `json:"original_request,omitempty"`
	ExecutorRequest *debugMessage `json:"executor_request,omitempty"`
	Request         debugMessage  `json:"request"`
	Response        debugResponse `json:"response"`
	secret          string        `json:"-"`
	secrets         []string      `json:"-"`
}

type debugMessage struct {
	Format  string      `json:"format,omitempty"`
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    string      `json:"body,omitempty"`
}

type debugResponse struct {
	StatusCode int         `json:"status_code,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       string      `json:"body,omitempty"`
	Complete   bool        `json:"complete"`
	Error      string      `json:"error,omitempty"`
}

func debugModeEnabled() bool {
	loadState()
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state.DebugMode
}

func updateDebugMode(body []byte) ([]byte, error) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(body, &input) != nil || input.Enabled == nil {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "enabled must be a boolean"})
	}
	err := mutateState(func(state *pluginState) error {
		state.DebugMode = *input.Enabled
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(map[string]bool{"debug_mode": *input.Enabled})
}

func queryDebugLog(query url.Values) ([]byte, error) {
	id := strings.TrimSpace(query.Get("id"))
	path, err := debugLogPath(id)
	if err != nil {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "debug log id is invalid"})
	}
	debugMu.RLock()
	raw, err := os.ReadFile(path)
	debugMu.RUnlock()
	if errors.Is(err, os.ErrNotExist) {
		return managementJSONStatus(http.StatusNotFound, map[string]string{"error": "debug data is no longer available"})
	}
	if err != nil {
		return managementJSONStatus(http.StatusInternalServerError, map[string]string{"error": "could not read debug data"})
	}
	return managementResult(http.StatusOK, raw)
}

func debugLogExists(id string) bool {
	path, err := debugLogPath(id)
	if err != nil {
		return false
	}
	debugMu.RLock()
	_, err = os.Stat(path)
	debugMu.RUnlock()
	return err == nil
}

func saveDebugLog(exchange *debugExchange, secret string) error {
	if exchange == nil {
		return nil
	}
	path, err := debugLogPath(exchange.ID)
	if err != nil {
		return err
	}
	stored := *exchange
	stored.secret = ""
	stored.secrets = nil
	secrets := append(append([]string(nil), exchange.secrets...), secret)
	redact := func(value string) string {
		for _, key := range secrets {
			value = redactDebugValue(value, key)
		}
		return value
	}
	redactMessage := func(message debugMessage) debugMessage {
		message.URL = redact(message.URL)
		message.Headers = redactDebugHeaders(message.Headers, "")
		for name, values := range message.Headers {
			for i, value := range values {
				message.Headers[name][i] = redact(value)
			}
		}
		message.Body = redact(message.Body)
		return message
	}
	if exchange.OriginalRequest != nil {
		message := redactMessage(*exchange.OriginalRequest)
		stored.OriginalRequest = &message
	}
	if exchange.ExecutorRequest != nil {
		message := redactMessage(*exchange.ExecutorRequest)
		stored.ExecutorRequest = &message
	}
	stored.Request = redactMessage(stored.Request)
	stored.Response.Headers = redactDebugHeaders(stored.Response.Headers, "")
	for name, values := range stored.Response.Headers {
		for i, value := range values {
			stored.Response.Headers[name][i] = redact(value)
		}
	}
	stored.Response.Body = redact(stored.Response.Body)
	stored.Response.Error = redact(stored.Response.Error)
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxDebugLogBytes {
		return errors.New("debug exchange exceeds the storage limit")
	}

	debugMu.Lock()
	defer debugMu.Unlock()
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return pruneDebugLogs(directory, path)
}

func clearDebugLogs() error {
	stateFile, err := statePath()
	if err != nil {
		return err
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	return os.RemoveAll(filepath.Join(filepath.Dir(stateFile), debugDirectory))
}

func debugLogPath(id string) (string, error) {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 12 {
		return "", errors.New("invalid debug log id")
	}
	stateFile, err := statePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(stateFile), debugDirectory, id+".json"), nil
}

func pruneDebugLogs(directory, current string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	files := make([]fileInfo, 0, len(entries))
	var totalBytes int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := fileInfo{path: filepath.Join(directory, entry.Name()), modTime: info.ModTime(), size: info.Size()}
		files = append(files, file)
		totalBytes += file.size
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for len(files) > maxDebugLogs || totalBytes > maxDebugLogBytes {
		oldest := 0
		if files[oldest].path == current {
			if len(files) == 1 {
				break
			}
			oldest = 1
		}
		if err := os.Remove(files[oldest].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		totalBytes -= files[oldest].size
		files = append(files[:oldest], files[oldest+1:]...)
	}
	return nil
}

func redactDebugHeaders(headers http.Header, secret string) http.Header {
	if headers == nil {
		return nil
	}
	redacted := make(http.Header, len(headers))
	for name, values := range headers {
		lowerName := strings.ToLower(name)
		if sensitiveDroppedName(name) || strings.Contains(lowerName, "token") {
			redacted[name] = []string{debugRedactedText}
			continue
		}
		redactedValues := make([]string, len(values))
		for index, value := range values {
			redactedValues[index] = redactDebugValue(value, secret)
		}
		redacted[name] = redactedValues
	}
	return redacted
}

func redactDebugValue(value, secret string) string {
	if secret != "" {
		value = strings.ReplaceAll(value, secret, debugRedactedText)
	}
	return value
}
