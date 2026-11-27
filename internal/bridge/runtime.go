// Package bridge implements the Mistral Bridge plugin independently of its C ABI adapter.
package bridge

import (
	"encoding/json"
	"fmt"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
}

// HostTransport exchanges raw JSON envelopes with the plugin host.
// The adapter owns and releases any native buffers before returning.
type HostTransport func(method string, request []byte) (response []byte, code int, err error)

var hostTransport HostTransport = func(string, []byte) ([]byte, int, error) {
	return nil, 1, nil
}

// Initialize installs the host transport before the host starts making plugin calls.
func Initialize(transport HostTransport) { hostTransport = transport }

// Shutdown releases the plugin's proxy transports.
func Shutdown() { closeTelemetry(); closeProxyTransport() }

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode plugin response: %w", err)
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

// ErrorEnvelope encodes a protocol error for the C ABI adapter.
func ErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &rpcError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func callHost(method string, payload any) (json.RawMessage, error) {
	request, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode host request: %w", err)
	}
	raw, callCode, err := hostTransport(method, request)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response (code %d)", method, int(callCode))
	}
	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode host callback %s: %w", method, err)
	}
	if !result.OK {
		if result.Error != nil {
			return nil, fmt.Errorf("host callback %s: %s", method, result.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code %d", method, int(callCode))
	}
	return append(json.RawMessage(nil), result.Result...), nil
}
