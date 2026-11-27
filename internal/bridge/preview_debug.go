package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxPreviewRequestBytes = 1024 * 1024

type previewDebugRecord struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	Model             string    `json:"model"`
	UpstreamModel     string    `json:"upstream_model,omitempty"`
	KeyLabel          string    `json:"key_label,omitempty"`
	Stream            bool      `json:"stream"`
	Status            string    `json:"status"`
	HTTPStatus        int       `json:"http_status,omitempty"`
	OriginalAvailable bool      `json:"original_available"`
	ExecutorAvailable bool      `json:"executor_available"`
}

func previewRecord(row RequestLog) previewDebugRecord {
	return previewDebugRecord{ID: row.ID, CreatedAt: row.CreatedAt, Model: row.Model, UpstreamModel: row.UpstreamModel, KeyLabel: row.KeyLabel, Stream: row.Stream, Status: row.Status, HTTPStatus: row.HTTPStatus, OriginalAvailable: row.OriginalRequestAvailable, ExecutorAvailable: row.ExecutorRequestAvailable}
}

// Lists metadata only; prompts and response bodies are not loaded into the picker.
func queryPreviewDebugRecords(query url.Values) ([]byte, error) {
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	page, _ := strconv.Atoi(query.Get("page"))
	size, _ := strconv.Atoi(query.Get("page_size"))
	page = max(1, min(page, maxRequestLogs))
	if size <= 0 {
		size = 10
	}
	size = min(size, 50)
	stateMu.RLock()
	logs := append([]RequestLog(nil), state.Logs...)
	stateMu.RUnlock()
	records := []previewDebugRecord{}
	for i := len(logs) - 1; i >= 0; i-- {
		row := logs[i]
		if !row.DebugAvailable || !debugLogExists(row.ID) {
			continue
		}
		text := strings.ToLower(strings.Join([]string{row.ID, row.Model, row.UpstreamModel, row.KeyLabel, row.Status, strconv.Itoa(row.HTTPStatus), row.CreatedAt.Format(time.RFC3339)}, " "))
		if search != "" && !strings.Contains(text, search) {
			continue
		}
		records = append(records, previewRecord(row))
	}
	pages := max(1, (len(records)+size-1)/size)
	page = min(page, pages)
	start := min((page-1)*size, len(records))
	end := min(start+size, len(records))
	return managementJSON(map[string]any{"records": records[start:end], "total": len(records), "page": page, "pages": pages, "page_size": size})
}

func importPreviewDebugRecord(query url.Values) ([]byte, error) {
	id := strings.TrimSpace(query.Get("id"))
	kind := query.Get("kind")
	if kind == "" {
		kind = "original_request"
	}
	field := ""
	switch kind {
	case "original_request", "executor_request":
		field = kind
	case "upstream_request":
		field = "request"
	default:
		return managementJSONStatus(400, map[string]string{"error": "请求来源须为 original_request、executor_request 或 upstream_request"})
	}
	path, err := debugLogPath(id)
	if err != nil {
		return managementJSONStatus(400, map[string]string{"error": "调试记录 ID 无效"})
	}
	debugMu.RLock()
	file, err := os.Open(path)
	if err != nil {
		debugMu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			return managementJSONStatus(404, map[string]string{"error": "调试记录已过期或已被清理，请刷新列表"})
		}
		return managementJSONStatus(500, map[string]string{"error": "无法读取调试请求"})
	}
	// Request snapshots precede response. Stop as soon as the selected snapshot is decoded
	// so importing a small request never reads a potentially huge SSE response.
	request, err := readPreviewDebugRequest(io.LimitReader(file, 24*maxPreviewRequestBytes), field)
	_ = file.Close()
	debugMu.RUnlock()
	if errors.Is(err, errDebugRequestMissing) {
		return managementJSONStatus(409, map[string]string{"error": "此记录未保存所选请求来源；旧版记录无法恢复原始请求，请选择其他来源或在调试模式下重新请求"})
	}
	if err != nil {
		return managementJSONStatus(422, map[string]string{"error": "此记录没有可导入的 JSON 请求，或请求超过预览大小限制"})
	}
	if kind == "upstream_request" && request.Method == "" || !bytes.HasPrefix(bytes.TrimSpace([]byte(request.Body)), []byte("{")) || !json.Valid([]byte(request.Body)) {
		return managementJSONStatus(422, map[string]string{"error": "此记录的所选来源没有可导入的 JSON 对象请求"})
	}
	previewInput, err := json.Marshal(map[string]any{"body": json.RawMessage(request.Body), "headers": request.Headers})
	if err != nil || len(previewInput) > maxPreviewRequestBytes {
		return managementJSONStatus(413, map[string]string{"error": "调试请求超过预览上限（1 MiB），请手动选取需要的字段"})
	}
	source := previewDebugRecord{ID: id}
	stateMu.RLock()
	for _, row := range state.Logs {
		if row.ID == id {
			source = previewRecord(row)
			break
		}
	}
	stateMu.RUnlock()
	return managementJSON(map[string]any{"source": source, "body_json": request.Body, "headers": request.Headers, "kind": kind, "format": request.Format})
}

var errDebugRequestMissing = errors.New("requested debug snapshot was not captured")

func readPreviewDebugRequest(reader io.Reader, selected string) (debugMessage, error) {
	decoder := json.NewDecoder(reader)
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return debugMessage{}, errors.New("invalid debug record")
	}
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return debugMessage{}, err
		}
		if field == selected {
			var request *debugMessage
			err := decoder.Decode(&request)
			if err != nil {
				return debugMessage{}, err
			}
			if request == nil {
				return debugMessage{}, errDebugRequestMissing
			}
			return *request, nil
		}
		if field == "response" {
			return debugMessage{}, errDebugRequestMissing
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return debugMessage{}, err
		}
	}
	return debugMessage{}, errDebugRequestMissing
}
