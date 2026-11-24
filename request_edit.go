package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	maxRequestEditRules = 100
	maxEditPathMatches  = 4096
)

type requestEditConfig struct {
	BuiltinEnabled       bool              `json:"builtin_enabled"`
	AutoDropIncompatible *bool             `json:"auto_drop_incompatible,omitempty"`
	Rules                []requestEditRule `json:"rules"`
}

type requestEditRule struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Action    string          `json:"action"`
	Source    string          `json:"source,omitempty"`
	From      string          `json:"from,omitempty"`
	Target    string          `json:"target"`
	Path      string          `json:"path"`
	ValueJSON string          `json:"value_json,omitempty"`
	Value     json.RawMessage `json:"-"`
	Overwrite bool            `json:"overwrite"`
}

func cloneRequestEdit(config *requestEditConfig) *requestEditConfig {
	copy := &requestEditConfig{BuiltinEnabled: true, Rules: []requestEditRule{}}
	enabled := autoDropEnabled(config)
	copy.AutoDropIncompatible = &enabled
	if config != nil {
		copy.BuiltinEnabled = config.BuiltinEnabled
		for _, rule := range config.Rules {
			rule.Value = append(json.RawMessage(nil), rule.Value...)
			copy.Rules = append(copy.Rules, rule)
		}
	}
	return copy
}

func updateRequestEdit(raw []byte) ([]byte, error) {
	var input struct {
		BuiltinEnabled       *bool              `json:"builtin_enabled"`
		AutoDropIncompatible *bool              `json:"auto_drop_incompatible"`
		Rules                *[]requestEditRule `json:"rules"`
	}
	if len(raw) > 1024*1024 || decodeEditJSON(raw, &input) != nil || (input.BuiltinEnabled == nil && input.AutoDropIncompatible == nil && input.Rules == nil) {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "请提供内置映射、自动丢弃开关或自定义规则列表"})
	}
	if input.Rules != nil {
		if err := validateRequestEditRules(*input.Rules); err != nil {
			return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	err := mutateState(func(state *pluginState) error {
		config := cloneRequestEdit(state.RequestEdit)
		if input.BuiltinEnabled != nil {
			config.BuiltinEnabled = *input.BuiltinEnabled
		}
		if input.AutoDropIncompatible != nil {
			config.AutoDropIncompatible = input.AutoDropIncompatible
		}
		if input.Rules != nil {
			config.Rules = *input.Rules
			for i := range config.Rules {
				if config.Rules[i].ID == "" {
					config.Rules[i].ID = randomID()
				}
			}
		}
		state.RequestEdit = cloneRequestEdit(config)
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(currentState().RequestEdit)
}

func decodeEditJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected a single JSON value")
	}
	return nil
}

func validateRequestEditRules(rules []requestEditRule) error {
	if len(rules) > maxRequestEditRules {
		return fmt.Errorf("最多保存 %d 条规则", maxRequestEditRules)
	}
	ids := map[string]bool{}
	for i := range rules {
		rule := &rules[i]
		rule.Name = strings.TrimSpace(rule.Name)
		rule.From = strings.TrimSpace(rule.From)
		rule.Path = strings.TrimSpace(rule.Path)
		fail := func(reason string) error { return fmt.Errorf("规则 %d：%s", i+1, reason) }
		if rule.Name == "" || len(rule.Name) > 160 || len(rule.ID) > 128 {
			return fail("请填写规则名称（最多 160 字节）")
		}
		if rule.ID != "" && ids[rule.ID] {
			return fail("规则 ID 重复")
		}
		ids[rule.ID] = true
		if rule.Action != "set" && rule.Action != "copy" && rule.Action != "move" && rule.Action != "delete" {
			return fail("操作必须是 set、copy、move 或 delete")
		}
		path, err := validateEditField(rule.Target, rule.Path, true)
		if err != nil {
			return fail(err.Error())
		}
		if rule.Action == "copy" || rule.Action == "move" {
			from, err := validateEditField(rule.Source, rule.From, rule.Action == "move")
			if err != nil {
				return fail("来源：" + err.Error())
			}
			if countWildcards(from) != countWildcards(path) {
				return fail("复制或移动时，来源和目标的 * 数量必须相同")
			}
			if rule.Action == "move" && rule.Source == "body" && rule.Target == "body" && rule.From != rule.Path && (pointerPrefix(from, path) || pointerPrefix(path, from)) {
				return fail("不能在父字段和其子字段之间移动")
			}
		} else {
			rule.Source, rule.From = "", ""
		}
		if rule.Action == "set" {
			if rule.ValueJSON != "" {
				rule.Value = json.RawMessage(rule.ValueJSON)
			}
			var value any
			if len(rule.Value) > 65536 || decodeEditJSON(rule.Value, &value) != nil {
				return fail("设置值必须是合法 JSON，且不超过 64 KiB")
			}
			rule.ValueJSON = string(bytes.TrimSpace(rule.Value))
			if rule.Target == "header" {
				if _, err := editHeaderValues(value); err != nil {
					return fail(err.Error())
				}
			}
		} else {
			rule.Value = nil
			rule.ValueJSON = ""
		}
	}
	return nil
}

func validateEditField(scope, field string, writing bool) ([]string, error) {
	if scope == "header" {
		if !validEditHeaderName(field) {
			return nil, errors.New("标头名称无效")
		}
		if writing && protectedEditHeader(field) {
			return nil, errors.New("鉴权、内容类型及 HTTP 传输标头由插件管理，不能改写")
		}
		return nil, nil
	}
	if scope != "body" {
		return nil, errors.New("字段位置必须是 body 或 header")
	}
	path, err := parseEditPointer(field)
	if err != nil {
		return nil, err
	}
	if writing && (path[0] == "model" || path[0] == "stream" || path[0] == "*") {
		return nil, errors.New("model 和 stream 由模型路由及请求模式决定，不能改写")
	}
	for _, part := range path {
		if part == "-" {
			return nil, errors.New("数组路径请使用索引或 *，不支持追加标记 -")
		}
	}
	return path, nil
}

func parseEditPointer(field string) ([]string, error) {
	if !strings.HasPrefix(field, "/") || len(field) > 1024 {
		return nil, errors.New("请求体路径须使用 JSON Pointer，例如 /metadata/project 或 /messages/*/content")
	}
	parts := strings.Split(field[1:], "/")
	if len(parts) > 32 {
		return nil, errors.New("字段路径最多 32 层")
	}
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 == len(part) || (part[j+1] != '0' && part[j+1] != '1') {
					return nil, errors.New("路径中的 ~ 须转义为 ~0，/ 须转义为 ~1")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func pointerPrefix(parent, child []string) bool {
	if len(parent) >= len(child) {
		return false
	}
	for i := range parent {
		if parent[i] != child[i] {
			return false
		}
	}
	return true
}

func countWildcards(path []string) int {
	count := 0
	for _, part := range path {
		if part == "*" {
			count++
		}
	}
	return count
}

func validEditHeaderName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
			return false
		}
	}
	return true
}

func protectedEditHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "host", "content-type", "accept", "content-length", "connection", "transfer-encoding", "keep-alive", "te", "trailer", "upgrade":
		return true
	}
	return false
}

func editHeaderValues(value any) ([]string, error) {
	var values []string
	switch value := value.(type) {
	case string:
		values = []string{value}
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("标头值须为 JSON 字符串或字符串数组")
			}
			values = append(values, text)
		}
	default:
		return nil, errors.New("标头值须为 JSON 字符串或字符串数组")
	}
	if len(values) == 0 {
		return nil, errors.New("标头值数组不能为空，请使用删除操作移除标头")
	}
	for _, value := range values {
		for _, char := range value {
			if char < 32 && char != '\t' || char == 127 {
				return nil, errors.New("标头值不能包含换行或控制字符")
			}
		}
	}
	return values, nil
}

type editPathMatch struct {
	Path     []string
	Captures []string
	Value    any
}

func editPathMatches(root any, path []string, missingLeaf bool) ([]editPathMatch, error) {
	var matches []editPathMatch
	var walk func(any, []string, []string, []string) error
	walk = func(node any, remaining, concrete, captures []string) error {
		if len(remaining) == 0 {
			if len(matches) >= maxEditPathMatches {
				return errors.New("通配符匹配过多，最多 4096 个字段")
			}
			matches = append(matches, editPathMatch{append([]string(nil), concrete...), append([]string(nil), captures...), node})
			return nil
		}
		part := remaining[0]
		if part == "*" {
			var keys []string
			switch node := node.(type) {
			case map[string]any:
				for key := range node {
					keys = append(keys, key)
				}
				sort.Strings(keys)
			case []any:
				for i := range node {
					keys = append(keys, strconv.Itoa(i))
				}
			}
			for _, key := range keys {
				child, _ := editChild(node, key)
				if err := walk(child, remaining[1:], append(concrete, key), append(captures, key)); err != nil {
					return err
				}
			}
			return nil
		}
		child, exists := editChild(node, part)
		if !exists && missingLeaf && countWildcards(remaining) == 0 {
			return walk(nil, nil, append(concrete, remaining...), captures)
		}
		if exists {
			return walk(child, remaining[1:], append(concrete, part), captures)
		}
		return nil
	}
	err := walk(root, path, nil, nil)
	return matches, err
}

func editChild(node any, key string) (any, bool) {
	switch node := node.(type) {
	case map[string]any:
		value, ok := node[key]
		return value, ok
	case []any:
		index, err := strconv.Atoi(key)
		if err == nil && index >= 0 && index < len(node) && strconv.Itoa(index) == key {
			return node[index], true
		}
	}
	return nil, false
}

func cloneEditValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(value))
		for key, child := range value {
			copy[key] = cloneEditValue(child)
		}
		return copy
	case []any:
		copy := make([]any, len(value))
		for i, child := range value {
			copy[i] = cloneEditValue(child)
		}
		return copy
	}
	return value
}

func writeEditBody(node any, path []string, value any, overwrite, deleting bool) (any, bool, error) {
	part := path[0]
	last := len(path) == 1
	if node == nil && !deleting {
		if index, err := strconv.Atoi(part); err == nil && index >= 0 && strconv.Itoa(index) == part {
			node = []any{}
		} else {
			node = map[string]any{}
		}
	}
	switch current := node.(type) {
	case map[string]any:
		child, exists := current[part]
		if last {
			if deleting {
				delete(current, part)
				return current, exists, nil
			}
			if exists && !overwrite {
				return current, false, nil
			}
			current[part] = cloneEditValue(value)
			return current, true, nil
		}
		if !exists && deleting {
			return current, false, nil
		}
		child, changed, err := writeEditBody(child, path[1:], value, overwrite, deleting)
		if err == nil && changed {
			current[part] = child
		}
		return current, changed, err
	case []any:
		index, err := strconv.Atoi(part)
		if err != nil || index < 0 || index >= maxEditPathMatches || strconv.Itoa(index) != part {
			return node, false, errors.New("数组索引须为 0 到 4095 的整数")
		}
		exists := index < len(current)
		if !exists {
			if deleting {
				return current, false, nil
			}
			current = append(current, make([]any, index+1-len(current))...)
		}
		if last {
			if deleting {
				return append(current[:index], current[index+1:]...), true, nil
			}
			if exists && !overwrite {
				return current, false, nil
			}
			current[index] = cloneEditValue(value)
			return current, true, nil
		}
		child, changed, err := writeEditBody(current[index], path[1:], value, overwrite, deleting)
		if err == nil && changed {
			current[index] = child
		}
		return current, changed, err
	default:
		if deleting {
			return node, false, nil
		}
		return node, false, errors.New("目标路径的父字段不是对象或数组")
	}
}

// Incoming headers are rule sources. Only explicitly written/copied headers are
// forwarded; client credentials, cookies and unrelated headers stay at CPA.
func applyCustomRequestRules(raw []byte, incoming http.Header, rules []requestEditRule) ([]byte, http.Header, error) {
	var body map[string]any
	if decodeEditJSON(raw, &body) != nil || body == nil {
		return nil, nil, errors.New("请求体必须是 JSON 对象")
	}
	working := http.Header{}
	names := make([]string, 0, len(incoming))
	for name := range incoming {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		canonical := http.CanonicalHeaderKey(name)
		working[canonical] = append(working[canonical], incoming[name]...)
	}
	outgoing := http.Header{}
	for i, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if err := applyRequestEditRule(body, working, outgoing, rule); err != nil {
			return nil, nil, fmt.Errorf("规则 %d（%s）：%w", i+1, rule.Name, err)
		}
	}
	encoded, err := json.Marshal(body)
	return encoded, outgoing, err
}

func applyRequestEditRule(body map[string]any, working, outgoing http.Header, rule requestEditRule) error {
	path, _ := parseEditPointer(rule.Path)
	var matches []editPathMatch
	switch rule.Action {
	case "copy", "move":
		if rule.Source == "header" {
			values, exists := working[http.CanonicalHeaderKey(rule.From)]
			if exists && len(values) > 0 {
				var value any = values[0]
				if len(values) > 1 {
					items := make([]any, len(values))
					for i, text := range values {
						items[i] = text
					}
					value = items
				}
				matches = []editPathMatch{{Value: value}}
			}
		} else {
			from, _ := parseEditPointer(rule.From)
			var err error
			matches, err = editPathMatches(body, from, false)
			if err != nil {
				return err
			}
			for i := range matches {
				matches[i].Value = cloneEditValue(matches[i].Value)
			}
		}
	case "set", "delete":
		var value any
		if rule.Action == "set" {
			if err := decodeEditJSON(rule.Value, &value); err != nil {
				return errors.New("设置值不是合法 JSON")
			}
		}
		if rule.Target == "body" && countWildcards(path) > 0 {
			var err error
			matches, err = editPathMatches(body, path, rule.Action == "set")
			if err != nil {
				return err
			}
			for i := range matches {
				matches[i].Value = value
			}
		} else {
			matches = []editPathMatch{{Path: path, Value: value}}
		}
	}
	// Reverse traversal keeps array indices stable when deleting or moving.
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		target := append([]string(nil), path...)
		capture := 0
		for j := range target {
			if target[j] == "*" {
				target[j] = match.Captures[capture]
				capture++
			}
		}
		if rule.Action == "move" && rule.Source == "body" && rule.Target == "body" && (pointerPrefix(match.Path, target) || pointerPrefix(target, match.Path)) {
			return errors.New("不能在父字段和其子字段之间移动")
		}
		changed := false
		if rule.Target == "header" {
			name := http.CanonicalHeaderKey(rule.Path)
			if rule.Action == "delete" {
				delete(working, name)
				delete(outgoing, name)
			} else if _, exists := outgoing[name]; rule.Overwrite || !exists {
				values, err := editHeaderValues(match.Value)
				if err != nil {
					return err
				}
				working[name] = append([]string(nil), values...)
				outgoing[name] = values
				changed = true
			}
		} else {
			var err error
			_, changed, err = writeEditBody(body, target, match.Value, rule.Overwrite, rule.Action == "delete")
			if err != nil {
				return err
			}
		}
		if rule.Action == "move" && changed && !(rule.Source == rule.Target && (rule.Source == "body" && rule.From == rule.Path || rule.Source == "header" && strings.EqualFold(rule.From, rule.Path))) {
			if rule.Source == "header" {
				working.Del(rule.From)
				outgoing.Del(rule.From)
			} else if _, _, err := writeEditBody(body, match.Path, nil, true, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// These aliases describe equivalent sampling controls, not whole-protocol
// conversion. CPA still translates Anthropic/Responses/Gemini message formats.
func applyBuiltinFieldMappings(root map[string]json.RawMessage) {
	aliases := []struct {
		native string
		from   []string
	}{
		{"max_tokens", []string{"max_completion_tokens", "max_output_tokens", "maxOutputTokens"}},
		{"random_seed", []string{"seed"}},
		{"stop", []string{"stop_sequences", "stopSequences"}},
		{"top_p", []string{"topP"}},
		{"top_k", []string{"topK"}},
	}
	for _, alias := range aliases {
		for _, from := range alias.from {
			setBuiltinDefault(root, alias.native, root[from])
			delete(root, from)
		}
	}
	var generation map[string]json.RawMessage
	if json.Unmarshal(root["generationConfig"], &generation) == nil && generation != nil {
		for from, to := range map[string]string{"maxOutputTokens": "max_tokens", "temperature": "temperature", "topP": "top_p", "topK": "top_k", "stopSequences": "stop", "seed": "random_seed", "candidateCount": "n"} {
			setBuiltinDefault(root, to, generation[from])
			delete(generation, from)
		}
		if len(generation) == 0 {
			delete(root, "generationConfig")
		} else {
			root["generationConfig"], _ = json.Marshal(generation)
		}
	}
	var reasoning map[string]json.RawMessage
	if json.Unmarshal(root["reasoning"], &reasoning) == nil && reasoning != nil {
		setBuiltinDefault(root, "reasoning_effort", reasoning["effort"])
		delete(reasoning, "effort")
		delete(reasoning, "summary") // Responses-only output configuration.
		if len(reasoning) == 0 {
			delete(root, "reasoning")
		} else {
			root["reasoning"], _ = json.Marshal(reasoning)
		}
	}
	// Absent from the native ChatCompletionRequest schema (additionalProperties=false).
	for _, field := range []string{"store", "stream_options", "user", "logit_bias", "prompt_cache_retention", "safety_identifier"} {
		delete(root, field)
	}
}

func setBuiltinDefault(root map[string]json.RawMessage, field string, value json.RawMessage) {
	if len(bytes.TrimSpace(root[field])) == 0 || bytes.Equal(bytes.TrimSpace(root[field]), []byte("null")) {
		if len(bytes.TrimSpace(value)) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			root[field] = value
		}
	}
}

func editCompletionRequest(raw []byte, incoming http.Header, config *requestEditConfig) ([]byte, http.Header, error) {
	return editCompletionRequestWithReport(raw, incoming, config, newRequestDropReport(incoming, ""))
}

func editCompletionRequestWithReport(raw []byte, incoming http.Header, config *requestEditConfig, report *requestDropReport) ([]byte, http.Header, error) {
	settings := cloneRequestEdit(config)
	if err := validateRequestEditRules(settings.Rules); err != nil {
		return nil, nil, err
	}
	body, err := transformRequestWithBuiltins(raw, "", "", settings.BuiltinEnabled)
	if err != nil {
		return nil, nil, err
	}
	body, headers, err := applyCustomRequestRules(body, incoming, settings.Rules)
	if err == nil && autoDropEnabled(settings) {
		body, err = dropIncompatibleRequestFields(body, report)
	}
	return body, headers, err
}

func previewRequestEdit(raw []byte) ([]byte, error) {
	var input struct {
		Body    json.RawMessage            `json:"body"`
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if len(raw) > 1024*1024 || decodeEditJSON(raw, &input) != nil {
		return managementJSONStatus(400, map[string]string{"error": "请提供 JSON 请求体与标头对象"})
	}
	headers := http.Header{}
	for name, rawValue := range input.Headers {
		var value any
		if !validEditHeaderName(name) || decodeEditJSON(rawValue, &value) != nil {
			return managementJSONStatus(400, map[string]string{"error": "输入标头名称或值无效"})
		}
		values, err := editHeaderValues(value)
		if err != nil {
			return managementJSONStatus(400, map[string]string{"error": err.Error()})
		}
		headers[name] = values
	}
	report := newRequestDropReport(headers, "")
	body, editedHeaders, err := editCompletionRequestWithReport(input.Body, headers, currentState().RequestEdit, report)
	if err != nil {
		return managementJSONStatus(400, map[string]string{"error": err.Error()})
	}
	editedHeaders.Set("Content-Type", "application/json")
	editedHeaders.Set("Accept", "application/json")
	var content map[string]json.RawMessage
	_ = json.Unmarshal(body, &content)
	if bytes.Equal(bytes.TrimSpace(content["stream"]), []byte("true")) {
		editedHeaders.Set("Accept", "text/event-stream")
	}
	result, err := json.MarshalIndent(map[string]any{"body": json.RawMessage(body), "headers": editedHeaders, "dropped_count": report.Count, "dropped_fields": report.Fields}, "", "  ")
	if err != nil {
		return nil, err
	}
	return managementResult(http.StatusOK, result)
}
