package bridge

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

//go:embed data/request_schema.json
var requestSchemaJSON []byte

type requestFieldSchema struct {
	Ref                  string                         `json:"$ref"`
	Type                 string                         `json:"type"`
	Properties           map[string]*requestFieldSchema `json:"properties"`
	AdditionalProperties json.RawMessage                `json:"additionalProperties"`
	Items                *requestFieldSchema            `json:"items"`
	AnyOf                []*requestFieldSchema          `json:"anyOf"`
	OneOf                []*requestFieldSchema          `json:"oneOf"`
	Const                json.RawMessage                `json:"const"`
	Discriminator        *struct {
		Property string            `json:"propertyName"`
		Mapping  map[string]string `json:"mapping"`
	} `json:"discriminator"`
}

var mistralRequestSchema = func() struct {
	Root    string                         `json:"root"`
	Schemas map[string]*requestFieldSchema `json:"schemas"`
} {
	var schema struct {
		Root    string                         `json:"root"`
		Schemas map[string]*requestFieldSchema `json:"schemas"`
	}
	if err := json.Unmarshal(requestSchemaJSON, &schema); err != nil || schema.Schemas[schema.Root] == nil {
		panic("invalid bundled Mistral request field schema")
	}
	return schema
}()

type droppedRequestField struct {
	Path      string `json:"path"`
	Value     string `json:"value"`
	Reason    string `json:"reason"`
	Truncated bool   `json:"truncated,omitempty"`
}

type requestDropReport struct {
	Count   int                   `json:"dropped_count"`
	Fields  []droppedRequestField `json:"dropped_fields"`
	bytes   int
	secrets []string
}

const (
	maxDroppedFieldDetails = 64
	maxDroppedValueBytes   = 2048
	maxDroppedDetailBytes  = 16 * 1024
)

func autoDropEnabled(config *requestEditConfig) bool {
	return config == nil || config.AutoDropIncompatible == nil || *config.AutoDropIncompatible
}

func newRequestDropReport(incoming http.Header, secret string) *requestDropReport {
	report := &requestDropReport{Fields: []droppedRequestField{}}
	if secret != "" {
		report.secrets = append(report.secrets, secret)
	}
	for name, values := range incoming {
		if sensitiveDroppedName(name) {
			for _, value := range values {
				if value != "" {
					report.secrets = append(report.secrets, value)
					if scheme, token, ok := strings.Cut(value, " "); ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Basic")) && token != "" {
						report.secrets = append(report.secrets, token)
					}
				}
			}
		}
	}
	return report
}

func sensitiveDroppedName(name string) bool {
	name = strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(name))
	return strings.Contains(name, "authorization") || strings.Contains(name, "apikey") || strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "passwd") || strings.Contains(name, "cookie") || strings.Contains(name, "credential") || strings.Contains(name, "accesstoken") || strings.Contains(name, "refreshtoken") || name == "token"
}

func redactDroppedValue(value any, secrets []string) any {
	switch value := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(value))
		for key, child := range value {
			if sensitiveDroppedName(key) {
				copy[key] = debugRedactedText
			} else {
				copy[key] = redactDroppedValue(child, secrets)
			}
		}
		return copy
	case []any:
		copy := make([]any, len(value))
		for i, child := range value {
			copy[i] = redactDroppedValue(child, secrets)
		}
		return copy
	case string:
		for _, secret := range secrets {
			value = strings.ReplaceAll(value, secret, debugRedactedText)
		}
		return value
	}
	return value
}

func truncateDroppedText(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "…", true
}

func (r *requestDropReport) add(path, name string, value any) {
	r.Count++
	if len(r.Fields) >= maxDroppedFieldDetails || r.bytes >= maxDroppedDetailBytes {
		return
	}
	if sensitiveDroppedName(name) {
		value = debugRedactedText
	} else {
		value = redactDroppedValue(value, r.secrets)
	}
	encoded, _ := json.Marshal(value)
	text, truncated := truncateDroppedText(string(encoded), min(maxDroppedValueBytes, maxDroppedDetailBytes-r.bytes))
	path = redactDroppedValue(path, r.secrets).(string)
	path, pathTruncated := truncateDroppedText(path, 1024)
	r.Fields = append(r.Fields, droppedRequestField{Path: path, Value: text, Reason: "Mistral schema 不支持此字段", Truncated: truncated || pathTruncated})
	r.bytes += len(text) + len(path)
}

func resolveRequestFieldSchema(schema *requestFieldSchema) *requestFieldSchema {
	for i := 0; schema != nil && schema.Ref != "" && i < 32; i++ {
		schema = mistralRequestSchema.Schemas[strings.TrimPrefix(schema.Ref, "#/components/schemas/")]
	}
	return schema
}

func requestFieldSchemaMatches(schema *requestFieldSchema, value any) bool {
	schema = resolveRequestFieldSchema(schema)
	if schema == nil {
		return false
	}
	switch schema.Type {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return false
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return false
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "number", "integer":
		if _, ok := value.(json.Number); !ok {
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		return value == nil
	}
	if object, ok := value.(map[string]any); ok {
		for key, property := range schema.Properties {
			if len(property.Const) > 0 {
				if field, exists := object[key]; exists {
					encoded, _ := json.Marshal(field)
					if string(encoded) != string(property.Const) {
						return false
					}
				}
			}
		}
	}
	return true
}

func selectRequestFieldSchema(schema *requestFieldSchema, value any) *requestFieldSchema {
	for depth := 0; depth < 32; depth++ {
		schema = resolveRequestFieldSchema(schema)
		if schema == nil {
			return nil
		}
		if schema.Discriminator != nil {
			object, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			kind, _ := object[schema.Discriminator.Property].(string)
			schema = mistralRequestSchema.Schemas[strings.TrimPrefix(schema.Discriminator.Mapping[kind], "#/components/schemas/")]
			continue
		}
		variants := schema.AnyOf
		if len(variants) == 0 {
			variants = schema.OneOf
		}
		if len(variants) == 0 {
			return schema
		}
		var selected *requestFieldSchema
		for _, variant := range variants {
			if requestFieldSchemaMatches(variant, value) {
				if selected != nil {
					// Ambiguous/unknown variants keep their contents for upstream validation.
					return nil
				}
				selected = variant
			}
		}
		schema = selected
	}
	return nil
}

func filterRequestFields(value any, schema *requestFieldSchema, path string, report *requestDropReport, depth int) {
	if depth > 64 {
		return
	}
	schema = selectRequestFieldSchema(schema, value)
	if schema == nil {
		return
	}
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
			if child, ok := schema.Properties[key]; ok {
				filterRequestFields(value[key], child, childPath, report, depth+1)
			} else if string(schema.AdditionalProperties) == "false" {
				report.add(childPath, key, value[key])
				delete(value, key)
			}
		}
	case []any:
		for i, item := range value {
			filterRequestFields(item, schema.Items, path+"/"+strconv.Itoa(i), report, depth+1)
		}
	}
}

func dropIncompatibleRequestFields(raw []byte, report *requestDropReport) ([]byte, error) {
	var body map[string]any
	if err := decodeEditJSON(raw, &body); err != nil {
		return nil, err
	}
	filterRequestFields(body, mistralRequestSchema.Schemas[mistralRequestSchema.Root], "", report, 0)
	return json.Marshal(body)
}
