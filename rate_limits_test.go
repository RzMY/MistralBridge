package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rateTestState(t *testing.T, upstream string) (*completionScheduler, *time.Time) {
	t.Helper()
	isolatedState(t)
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	s := completionLimits
	s.now = func() time.Time { return now }
	state.Keys = []apiKey{{ID: "one", Secret: "mock-one", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: upstream, Enabled: true}}
	return s, &now
}

func reserveTest(s *completionScheduler, settings pluginState, model string, maxTokens int) (scheduledCompletion, error) {
	body, _ := json.Marshal(map[string]any{"messages": []any{}, "max_tokens": maxTokens})
	req := rpcExecutorRequest{Model: model, Payload: body}
	return s.prepare(settings, req, &requestTrace{})
}

func requireLimited(t *testing.T, err error) {
	t.Helper()
	var status *statusError
	if !errors.As(err, &status) || status.Status != 429 || !strings.Contains(status.Message, "retry after") {
		t.Fatalf("expected cooldown 429, got %v", err)
	}
}

func TestBuiltinRateLimits(t *testing.T) {
	if len(builtinRateLimits) != 23 {
		t.Fatal("incomplete downloaded table")
	}
	for name, limit := range builtinRateLimits {
		if limit.TPM <= 0 || limit.RPS <= 0 {
			t.Fatalf("invalid limit %s: %+v", name, limit)
		}
	}
	if builtinRateLimits["glm-5-2"] != (modelRateLimit{300000, .5}) || builtinRateLimits["zai-glm-5-3-flash"] != (modelRateLimit{10666666, 22.22}) {
		t.Fatal("snapshot differs from downloaded limits")
	}
}

func TestRateSelectionFallbackBindingAndSharedAliases(t *testing.T) {
	s, now := rateTestState(t, "glm-5-2")
	state.Keys = append(state.Keys, apiKey{ID: "two", Secret: "mock-two", Enabled: true, Priority: -1})
	first, err := reserveTest(s, state, "alias", 10)
	if err != nil || first.key.ID != "one" {
		t.Fatalf("primary: %+v %v", first, err)
	}
	first.reservation.finish(nil)
	second, err := reserveTest(s, state, "alias", 10)
	if err != nil || second.key.ID != "two" {
		t.Fatalf("fallback: %+v %v", second, err)
	}
	second.reservation.finish(nil)
	_, err = reserveTest(s, state, "alias", 10)
	requireLimited(t, err)
	*now = now.Add(2 * time.Second)
	state.Models = append(state.Models, modelMapping{ClientModel: "other-alias", UpstreamModel: "glm-5-2", KeyID: "one", Enabled: true})
	first, err = reserveTest(s, state, "alias", 10)
	if err != nil || first.key.ID != "one" {
		t.Fatal("highest priority did not recover")
	}
	first.reservation.finish(nil)
	state.Keys[0].Channel = "vibe"
	_, err = reserveTest(s, state, "other-alias", 10)
	requireLimited(t, err)
	state.Models[1].UpstreamModel = "mistral-large-2512"
	other, err := reserveTest(s, state, "other-alias", 10)
	if err != nil {
		t.Fatal("unrelated model was cooled")
	}
	other.reservation.finish(nil)
	state.Keys[0].Enabled = false
	_, err = reserveTest(s, state, "other-alias", 10)
	if err == nil {
		t.Fatal("explicit binding used another credential")
	}
}

func TestRateReservationsAreAtomic(t *testing.T) {
	s, _ := rateTestState(t, "glm-5-2")
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reserveTest(s, state, "alias", 10)
			if err == nil {
				admitted.Add(1)
				r.reservation.finish(nil)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent requests admitted: %d", admitted.Load())
	}
}

func TestTPMReservationUsageAndLongStream(t *testing.T) {
	s, now := rateTestState(t, "glm-5-2")
	first, err := reserveTest(s, state, "alias", 290000)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(61 * time.Second)
	_, err = reserveTest(s, state, "alias", 20000)
	requireLimited(t, err)
	first.reservation.finish(map[string]int{"prompt_tokens": 5, "completion_tokens": 10})
	second, err := reserveTest(s, state, "alias", 20000)
	if err != nil {
		t.Fatal("actual usage did not release excess reservation", err)
	}
	second.reservation.finish(map[string]int{"total_tokens": 300000})
	*now = now.Add(2 * time.Second)
	_, err = reserveTest(s, state, "alias", 10)
	requireLimited(t, err)
	*now = now.Add(58 * time.Second)
	third, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal("TPM did not expire", err)
	}
	third.reservation.finish(nil)
}

func TestRetryAfterAndBackoff(t *testing.T) {
	s, now := rateTestState(t, "unknown-upstream")
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {"0.25", 250 * time.Millisecond}, {now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second}, {"NaN", 0}, {"+Inf", 0}, {"-1", 0}, {"bad", 0}, {"1e100", 0}, {now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		if got := retryAfter(http.Header{"retry-after": {test.value}}, *now); got != test.want {
			t.Fatalf("Retry-After %s: %v", test.value, got)
		}
	}
	for _, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		r, err := reserveTest(s, state, "alias", 10)
		if err != nil {
			t.Fatal(err)
		}
		r.reservation.throttled(nil)
		r.reservation.finish(nil)
		if got := r.reservation.bucket.cooldown.Sub(*now); got != wait {
			t.Fatalf("backoff %v, want %v", got, wait)
		}
		_, err = reserveTest(s, state, "alias", 10)
		requireLimited(t, err)
		*now = now.Add(wait)
	}
	r, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal(err)
	}
	r.reservation.throttled(http.Header{"Retry-After": {"3"}})
	if r.reservation.bucket.cooldown.Sub(*now) != 3*time.Second {
		t.Fatal("server delay ignored")
	}
	// A concurrent successful response must not clear an existing 429 cooldown.
	r.reservation.finish(map[string]int{"total_tokens": 8})
	_, err = reserveTest(s, state, "alias", 10)
	requireLimited(t, err)
}

func TestCompletion429DoesNotRepeatUpstream(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "stream"}[streaming], func(t *testing.T) {
			_, now := rateTestState(t, "unknown-upstream")
			calls := 0
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				calls++
				if method == "host.http.do_stream" {
					return json.Marshal(upstreamStreamResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}, Body: []byte(`{"message":"limited"}`)})
				}
				if method != "host.http.do" {
					t.Fatalf("unexpected host call %s", method)
				}
				return json.Marshal(upstreamResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}, Body: []byte(`{"message":"limited"}`)})
			}
			run := func() error {
				req := rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[]}`)}
				if streaming {
					_, err := performCompletionStream(context.Background(), req, newRequestTrace(req))
					return err
				}
				_, _, err := performCompletion(context.Background(), req, newRequestTrace(req))
				return err
			}
			requireLimited(t, run())
			for i := 0; i < 20; i++ {
				requireLimited(t, run())
			}
			if calls != 1 {
				t.Fatalf("cooldown sent %d requests", calls)
			}
			*now = now.Add(3 * time.Second)
			requireLimited(t, run())
			if calls != 2 {
				t.Fatal("upstream not retried after expiry")
			}
		})
	}
}

func TestInvalidRequestDoesNotConsumeBudget(t *testing.T) {
	s, _ := rateTestState(t, "glm-5-2")
	req := rpcExecutorRequest{Model: "alias", Payload: []byte(`invalid`)}
	if _, err := s.prepare(state, req, &requestTrace{}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	r, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal("invalid request consumed budget", err)
	}
	r.reservation.finish(nil)
}

func TestRejectedRequestReleasesTPMButKeepsCooldown(t *testing.T) {
	_, now := rateTestState(t, "glm-5-2")
	calls := 0
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return json.Marshal(upstreamResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}})
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[],"usage":{"total_tokens":8}}`)})
	}
	req := rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[],"max_tokens":300000}`)}
	_, _, err := performCompletion(context.Background(), req, newRequestTrace(req))
	requireLimited(t, err)
	*now = now.Add(2 * time.Second)
	_, _, err = performCompletion(context.Background(), req, newRequestTrace(req))
	requireLimited(t, err)
	*now = now.Add(time.Second)
	if _, _, err = performCompletion(context.Background(), req, newRequestTrace(req)); err != nil || calls != 2 {
		t.Fatalf("rejected request retained TPM reservation: %d calls, %v", calls, err)
	}
}
