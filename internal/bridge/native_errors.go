package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"
)

func waitForScheduledSlot(ctx context.Context, until time.Time) error {
	if until.IsZero() || !until.After(time.Now()) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &statusError{499, "客户端已取消排队请求"}
	}
}

type providerError struct {
	*statusError
	Code              string
	RetryAfterSeconds int
}

func (e *providerError) Unwrap() error { return e.statusError }

func nativeUpstreamError(status int, headers http.Header, body []byte) error {
	err := upstreamStatusError(status, body).(*statusError)
	seconds := int(math.Ceil(retryAfter(headers, time.Now()).Seconds()))
	code := "mistral_bridge_error"
	if status == 429 {
		code = "rate_limited"
	}
	return &providerError{err, code, seconds}
}

// ExecutorErrorEnvelope preserves status, cooldown and error scope in the RPC response.
func ExecutorErrorEnvelope(err error) []byte {
	detail := rpcError{Code: "mistral_bridge_error", Message: err.Error(), HTTPStatus: 500}
	var status *statusError
	if errors.As(err, &status) {
		detail.HTTPStatus = status.Status
	}
	var provider *providerError
	if errors.As(err, &provider) {
		detail.Code, detail.RetryAfterSeconds = provider.Code, provider.RetryAfterSeconds
	} else if detail.HTTPStatus == 400 || detail.HTTPStatus == 422 {
		detail.Code = "request_scoped"
	} else if detail.HTTPStatus == 501 {
		detail.Code = "not_implemented"
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &detail})
	return raw
}

func emitNativeStreamError(streamID, message string) {
	_, _ = hostCall("host.stream.emit", map[string]any{"stream_id": streamID, "error": message, "error_detail": rpcError{Code: "stream_truncated", HTTPStatus: 502, Message: message}})
}
