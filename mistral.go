package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const defaultBaseURL = "https://api.mistral.ai/v1"

type rpcExecutorRequest struct {
	AuthID          string            `json:"AuthID"`
	AuthProvider    string            `json:"AuthProvider"`
	StorageJSON     []byte            `json:"StorageJSON"`
	AuthMetadata    map[string]any    `json:"AuthMetadata"`
	AuthAttributes  map[string]string `json:"AuthAttributes"`
	Model           string            `json:"Model"`
	Format          string            `json:"Format"`
	SourceFormat    string            `json:"SourceFormat"`
	OriginalRequest []byte            `json:"OriginalRequest"`
	Payload         []byte            `json:"Payload"`
	Stream          bool              `json:"Stream"`
	Headers         http.Header       `json:"Headers"`
	Query           url.Values        `json:"Query"`
	Metadata        map[string]any    `json:"Metadata"`
	HostCallbackID  string            `json:"host_callback_id,omitempty"`
	StreamID        string            `json:"stream_id,omitempty"`
}

type upstreamRequest struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body,omitempty"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type upstreamResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type upstreamStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
	Body       []byte      `json:"body,omitempty"`
	reader     io.ReadCloser
	cleanup    func()
}

type streamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

func performCompletion(ctx context.Context, req rpcExecutorRequest, trace *requestTrace) ([]byte, http.Header, error) {
	settings := currentState()
	prepared, err := completionLimits.prepare(settings, req, trace)
	if err != nil {
		return nil, nil, err
	}
	body, headers, endpoint := prepared.body, prepared.headers, prepared.endpoint
	defer prepared.reservation.finish(nil)
	callbackReq := upstreamRequest{Method: http.MethodPost, URL: endpoint, Headers: headers, Body: body, HostCallbackID: req.HostCallbackID}
	trace.captureRequest(callbackReq.Method, callbackReq.URL, callbackReq.Headers, callbackReq.Body)
	responseRaw, err := doUpstreamHTTP(ctx, callbackReq, prepared.proxyURL)
	if err != nil {
		trace.captureResponseError(err)
		return nil, nil, &statusError{Status: 502, Message: "Mistral API request failed"}
	}
	var response upstreamResponse
	if err = json.Unmarshal(responseRaw, &response); err != nil {
		trace.captureResponse(0, nil, responseRaw, true)
		return nil, nil, &statusError{Status: 502, Message: "Invalid response from HTTP bridge"}
	}
	trace.captureResponse(response.StatusCode, response.Headers, response.Body, true)
	if response.StatusCode == http.StatusTooManyRequests {
		limited := nativeUpstreamError(response.StatusCode, response.Headers, response.Body)
		prepared.reservation.finish(map[string]int{"total_tokens": 0})
		return nil, nil, limited
	}
	if response.StatusCode >= 400 {
		prepared.reservation.finish(map[string]int{"total_tokens": 0})
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, nil, nativeUpstreamError(response.StatusCode, response.Headers, response.Body)
	}
	prepared.reservation.finish(parseUsage(response.Body))
	result, err := normalizeCompletion(response.Body)
	return result, response.Headers, err
}

func performCompletionStream(ctx context.Context, req rpcExecutorRequest, trace *requestTrace) (http.Header, error) {
	settings := currentState()
	req.Stream = true
	prepared, err := completionLimits.prepare(settings, req, trace)
	if err != nil {
		return nil, err
	}
	body, headers, endpoint := prepared.body, prepared.headers, prepared.endpoint
	handedOff := false
	defer func() {
		if !handedOff {
			prepared.reservation.finish(nil)
		}
	}()
	trace.captureRequest(http.MethodPost, endpoint, headers, body)
	stream, err := openUpstreamHTTPStream(ctx, upstreamRequest{
		Method: http.MethodPost, URL: endpoint, Headers: headers,
		Body: body, HostCallbackID: req.HostCallbackID,
	}, prepared.proxyURL)
	if err != nil {
		trace.captureResponseError(err)
		return nil, &statusError{Status: 502, Message: "Mistral streaming request failed"}
	}
	trace.captureResponseHead(stream.StatusCode, stream.Headers)
	var limited error
	if stream.StatusCode == http.StatusTooManyRequests {
		limited = nativeUpstreamError(stream.StatusCode, stream.Headers, stream.Body)
	}
	if stream.StatusCode >= 400 {
		prepared.reservation.finish(map[string]int{"total_tokens": 0})
	}
	if stream.StatusCode < 200 || stream.StatusCode > 299 {
		body := append([]byte(nil), stream.Body...)
		trace.captureResponseChunk(stream.Body)
		if stream.opened() {
			if trace.capturesDebug() || stream.reader != nil {
				body = append(body, readStreamErrorBody(req, stream, trace)...)
			} else {
				stream.close(req.HostCallbackID)
			}
		} else {
			trace.completeResponse()
		}
		if limited != nil {
			return nil, limited
		}
		return nil, nativeUpstreamError(stream.StatusCode, stream.Headers, body)
	}
	if !stream.opened() {
		trace.captureResponseError(errors.New("Mistral stream was not opened"))
		return nil, &statusError{Status: 502, Message: "Mistral stream was not opened"}
	}
	handedOff = true
	go relayStream(req, stream, trace, prepared.reservation)
	return http.Header{"content-type": []string{"text/event-stream"}}, nil
}

func readStreamErrorBody(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace) []byte {
	defer stream.close(req.HostCallbackID)
	var body bytes.Buffer
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			trace.captureResponseError(err)
			return body.Bytes()
		}
		if chunk.Error != "" {
			trace.captureResponseError(errors.New(chunk.Error))
			return body.Bytes()
		}
		if len(chunk.Payload) > 0 {
			_, _ = body.Write(chunk.Payload)
			trace.captureResponseChunk(chunk.Payload)
		}
		if chunk.Done {
			trace.completeResponse()
			return body.Bytes()
		}
	}
}

func relayStream(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace, reservations ...*tokenReservation) {
	var streamErr error
	var usage map[string]int
	defer func() {
		for _, reservation := range reservations {
			reservation.finish(usage)
		}
		trace.finish(usage, streamErr)
	}()
	defer stream.close(req.HostCallbackID)
	defer hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
	var pending []byte
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			trace.captureResponseError(err)
			emitNativeStreamError(req.StreamID, "Mistral stream read failed")
			return
		}
		trace.captureResponseChunk(chunk.Payload)
		if chunk.Error != "" {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			trace.captureResponseError(errors.New(chunk.Error))
			emitNativeStreamError(req.StreamID, "Mistral stream failed")
			return
		}
		pending = append(pending, chunk.Payload...)
		for {
			lineEnd := bytes.IndexByte(pending, '\n')
			if lineEnd < 0 {
				break
			}
			line := append([]byte(nil), pending[:lineEnd+1]...)
			pending = pending[lineEnd+1:]
			if lineUsage := parseSSEUsage(line); lineUsage != nil {
				usage = lineUsage
			}
			processed, errLine := normalizeSSELine(line)
			if errLine != nil {
				streamErr = &statusError{502, "Mistral 流式响应中断"}
				trace.captureResponseError(errLine)
				emitNativeStreamError(req.StreamID, "Invalid Mistral stream event")
				if trace.capturesDebug() {
					if err := drainUpstreamStream(req, stream, trace); err != nil {
						trace.captureResponseError(err)
					}
				}
				return
			}
			if len(processed) > 0 {
				trace.observe(processed)
				if _, errEmit := hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": processed}); errEmit != nil {
					streamErr = &statusError{499, "客户端已断开连接"}
					if !trace.capturesDebug() {
						return
					}
					if err := drainUpstreamStream(req, stream, trace); err != nil {
						trace.captureResponseError(err)
					}
					return
				}
			}
		}
		if chunk.Done {
			trace.completeResponse()
			if len(pending) > 0 {
				if lineUsage := parseSSEUsage(pending); lineUsage != nil {
					usage = lineUsage
				}
				processed, errLine := normalizeSSELine(pending)
				if errLine == nil && len(processed) > 0 {
					trace.observe(processed)
					if _, err := hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": processed}); err != nil {
						streamErr = &statusError{499, "客户端已断开连接"}
					}
				} else if errLine != nil {
					streamErr = &statusError{502, "Mistral 流式响应中断"}
					trace.captureResponseError(errLine)
				}
			}
			return
		}
	}
}

func drainUpstreamStream(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace) error {
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			return err
		}
		if chunk.Error != "" {
			return errors.New(chunk.Error)
		}
		trace.captureResponseChunk(chunk.Payload)
		if chunk.Done {
			trace.completeResponse()
			return nil
		}
	}
}

func normalizeSSELine(line []byte) ([]byte, error) {
	trimmed := bytes.TrimSuffix(line, []byte("\n"))
	trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, nil
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return line, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if err := normalizeChoices(root, true); err != nil {
		return nil, err
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), out...), '\n'), nil
}

func normalizeCompletion(body []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, &statusError{Status: 502, Message: "Mistral returned invalid JSON"}
	}
	if err := normalizeChoices(root, false); err != nil {
		return nil, &statusError{Status: 502, Message: "Mistral returned an unsupported content shape"}
	}
	return json.Marshal(root)
}

func normalizeChoices(root map[string]json.RawMessage, streaming bool) error {
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(root["choices"], &choices); err != nil {
		return err
	}
	for _, choice := range choices {
		field := "message"
		if streaming {
			field = "delta"
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(choice[field], &message) != nil || message == nil {
			continue
		}
		content, exists := message["content"]
		if !exists || len(bytes.TrimSpace(content)) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content, &parts); err != nil {
			continue
		}
		var thinking, text strings.Builder
		for _, part := range parts {
			var kind string
			_ = json.Unmarshal(part["type"], &kind)
			if kind == "thinking" {
				var thoughtParts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(part["thinking"], &thoughtParts)
				for _, item := range thoughtParts {
					thinking.WriteString(item.Text)
				}
				continue
			}
			if kind == "text" {
				var value string
				_ = json.Unmarshal(part["text"], &value)
				text.WriteString(value)
			}
		}
		newContent, err := json.Marshal(text.String())
		if err != nil {
			return err
		}
		message["content"] = newContent
		if thinking.Len() > 0 {
			reasoning, _ := json.Marshal(thinking.String())
			message["reasoning_content"] = reasoning
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return err
		}
		choice[field] = encoded
	}
	encoded, err := json.Marshal(choices)
	if err != nil {
		return err
	}
	root["choices"] = encoded
	return nil
}

// Mistral meters metadata.call_source=vibe_code against the Vibe allowance.
// Studio uses the upstream API route by omitting this marker, not by changing
// its value: Mistral may classify any nonempty call_source as Vibe traffic.
func applyKeyChannel(raw []byte, key apiKey) ([]byte, error) {
	channel := activeChannel(key)
	if channel == "" {
		return raw, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	if value := root["metadata"]; len(value) > 0 {
		if err := json.Unmarshal(value, &metadata); err != nil {
			return nil, errors.New("metadata must be an object")
		}
		if metadata == nil {
			metadata = map[string]json.RawMessage{}
		}
	}
	if channel == "vibe" {
		metadata["call_source"] = json.RawMessage(`"vibe_code"`)
	} else {
		delete(metadata, "call_source")
	}
	if len(metadata) == 0 {
		delete(root, "metadata")
	} else {
		root["metadata"], _ = json.Marshal(metadata)
	}
	return json.Marshal(root)
}

func prepareCompletionRequest(req rpcExecutorRequest, key apiKey, model modelMapping, config *requestEditConfig) ([]byte, http.Header, *requestDropReport, error) {
	drops := newRequestDropReport(req.Headers, key.Secret)
	body, headers, err := editCompletionRequestWithReport(req.Payload, req.Headers, config, drops)
	if err == nil {
		body, err = transformRequestWithBuiltins(body, model.UpstreamModel, model.ThinkingMap, false)
	}
	if err == nil {
		body, err = applyKeyChannel(body, key)
	}
	if err == nil {
		body, err = setJSONField(body, "stream", req.Stream)
	}
	if err != nil {
		return nil, nil, drops, &statusError{Status: http.StatusBadRequest, Message: "请求编辑失败：" + err.Error()}
	}
	headers.Set("Authorization", "Bearer "+key.Secret)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	if req.Stream {
		headers.Set("Accept", "text/event-stream")
	}
	return body, headers, drops, nil
}

func transformRequest(raw []byte, model, thinkingMap string) ([]byte, error) {
	return transformRequestWithBuiltins(raw, model, thinkingMap, true)
}

func transformRequestWithBuiltins(raw []byte, model, thinkingMap string, builtinEnabled bool) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	if model != "" {
		modelJSON, _ := json.Marshal(model)
		root["model"] = modelJSON
	}
	if builtinEnabled {
		applyBuiltinFieldMappings(root)
	}
	if effort, ok := rawString(root["reasoning_effort"]); ok {
		if mapped := mapThinking(effort, thinkingMap); mapped != "" {
			root["reasoning_effort"], _ = json.Marshal(mapped)
		} else {
			delete(root, "reasoning_effort")
		}
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(root["messages"], &messages); err != nil || messages == nil {
		return nil, errors.New("messages must be an array")
	}
	for _, message := range messages {
		if message == nil {
			return nil, errors.New("messages must contain objects")
		}
		if !builtinEnabled {
			continue
		}
		// Client cache hints are not part of Mistral's content chunk schema.
		var contentParts []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &contentParts) == nil {
			changed := false
			for _, part := range contentParts {
				if _, exists := part["cache_control"]; exists {
					delete(part, "cache_control")
					changed = true
				}
			}
			if changed {
				message["content"], _ = json.Marshal(contentParts)
			}
		}
		var role string
		_ = json.Unmarshal(message["role"], &role)
		// Map OpenAI developer instructions to Mistral's system role.
		if role == "developer" {
			role = "system"
			message["role"] = json.RawMessage(`"system"`)
		}
		if role != "tool" {
			delete(message, "name")
		}
		if role == "assistant" {
			delete(message, "refusal")
		}
		var reasoning strings.Builder
		for _, field := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
			if rawReasoning, exists := message[field]; exists {
				reasoning.WriteString(extractReasoning(rawReasoning))
				delete(message, field)
			}
		}
		if role != "assistant" || strings.TrimSpace(reasoning.String()) == "" {
			continue
		}
		parts := make([]json.RawMessage, 0, 2)
		thinkingPart, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": []map[string]string{{"type": "text", "text": reasoning.String()}}})
		parts = append(parts, thinkingPart)
		content := bytes.TrimSpace(message["content"])
		if len(content) > 0 && content[0] == '[' {
			var existing []json.RawMessage
			if json.Unmarshal(content, &existing) == nil {
				parts = append(parts, existing...)
			}
		} else {
			var contentText string
			if json.Unmarshal(content, &contentText) == nil && contentText != "" {
				textPart, _ := json.Marshal(map[string]string{"type": "text", "text": contentText})
				parts = append(parts, textPart)
			}
		}
		message["content"], _ = json.Marshal(parts)
	}
	root["messages"], _ = json.Marshal(messages)
	return json.Marshal(root)
}

func extractReasoning(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var result strings.Builder
		for _, item := range items {
			result.WriteString(item.Text)
		}
		return result.String()
	}
	var detail struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &detail)
	return detail.Text
}

func mapThinking(value, mapping string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if mapping == "" {
		if value == "off" {
			return "none"
		}
		return value
	}
	for _, pair := range strings.Split(mapping, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(strings.TrimSpace(from), value) {
			return strings.ToLower(strings.TrimSpace(to))
		}
	}
	return value
}

func setJSONField(raw []byte, key string, value any) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	root[key], _ = json.Marshal(value)
	return json.Marshal(root)
}

func rawString(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func completionURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("base URL must be an absolute HTTP(S) URL without credentials")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func upstreamStatusError(status int, body []byte) error {
	message := "Mistral API rejected the request"
	var parsed struct {
		Message json.RawMessage `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		var nested struct {
			Message json.RawMessage `json:"message"`
			Detail  json.RawMessage `json:"detail"`
		}
		_ = json.Unmarshal(parsed.Error, &nested)
		var errorText string
		_ = json.Unmarshal(parsed.Error, &errorText)
		for _, text := range []string{upstreamErrorDetail(parsed.Message), upstreamErrorDetail(parsed.Detail), upstreamErrorDetail(nested.Message), upstreamErrorDetail(nested.Detail), errorText} {
			if strings.TrimSpace(text) != "" {
				message = text
				break
			}
		}
	}
	if message == "Mistral API rejected the request" {
		if text := strings.TrimSpace(string(body)); text != "" && len(text) <= 600 {
			message = text
		}
	}
	if status < 400 || status > 599 {
		status = 502
	}
	return &statusError{Status: status, Message: message}
}

func upstreamErrorDetail(raw json.RawMessage) string {
	if detail, ok := rawString(raw); ok {
		return detail
	}
	// Some Mistral routes wrap Pydantic validation details inside message.
	var wrapped struct {
		Message json.RawMessage `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &wrapped) == nil {
		for _, field := range []json.RawMessage{wrapped.Message, wrapped.Detail} {
			if detail := upstreamErrorDetail(field); strings.TrimSpace(detail) != "" {
				return detail
			}
		}
	}
	var details []struct {
		Location []json.RawMessage `json:"loc"`
		Message  string            `json:"msg"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return ""
	}
	var summaries []string
	for _, detail := range details {
		if strings.TrimSpace(detail.Message) == "" {
			continue
		}
		var location strings.Builder
		for _, segment := range detail.Location {
			if field, ok := rawString(segment); ok {
				// Pydantic includes long union type descriptions in validation paths.
				if strings.ContainsAny(field, "[]") {
					continue
				}
				if location.Len() > 0 {
					location.WriteByte('.')
				}
				location.WriteString(field)
			} else {
				var index int
				if json.Unmarshal(segment, &index) == nil {
					location.WriteByte('[')
					location.WriteString(strconv.Itoa(index))
					location.WriteByte(']')
				}
			}
		}
		message := detail.Message
		if location.Len() > 0 {
			message = location.String() + ": " + message
		}
		summaries = append(summaries, message)
		if len(summaries) == 3 {
			break
		}
	}
	return strings.Join(summaries, "; ")
}

func parseUsage(raw []byte) map[string]int {
	var root struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Usage == nil {
		return nil
	}
	usage := make(map[string]int)
	for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		var value int
		if json.Unmarshal(root.Usage[name], &value) == nil {
			usage[name] = max(0, value)
		}
	}
	var details struct {
		CachedTokens *int `json:"cached_tokens"`
	}
	_ = json.Unmarshal(root.Usage["prompt_tokens_details"], &details)
	var legacy *int
	_ = json.Unmarshal(root.Usage["num_cached_tokens"], &legacy)
	if details.CachedTokens != nil {
		usage["cached_tokens"] = max(0, *details.CachedTokens)
	}
	if legacy != nil {
		usage["cached_tokens"] = max(usage["cached_tokens"], *legacy)
	}
	return usage
}

func parseSSEUsage(line []byte) map[string]int {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	return parseUsage(data)
}
