package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
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
	return s.prepare(settings, testNativeRequest(req), &requestTrace{})
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

func TestRateSharedAliasesAndModelIsolation(t *testing.T) {
	s, now := rateTestState(t, "glm-5-2")
	first, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal(err)
	}
	first.reservation.finish(nil)
	state.Models = append(state.Models, modelMapping{ClientModel: "other", UpstreamModel: "glm-5-2", Enabled: true})
	other, err := reserveTest(s, state, "other", 10)
	if err != nil || other.waitUntil.Sub(*now) != 2*time.Second {
		t.Fatalf("RPS burst should queue at the next slot: %+v %v", other, err)
	}
	other.reservation.finish(nil)
	*now = now.Add(2 * time.Second)
	state.Models[1].UpstreamModel = "mistral-large-2512"
	r, err := reserveTest(s, state, "other", 10)
	if err != nil {
		t.Fatal(err)
	}
	r.reservation.finish(nil)
	*now = now.Add(2 * time.Second)
	r, err = reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal(err)
	}
	r.reservation.finish(nil)
}

func TestRateReservationsAreAtomicallyPaced(t *testing.T) {
	s, _ := rateTestState(t, "glm-5-2")
	var admitted atomic.Int32
	var slotsMu sync.Mutex
	var slots []time.Time
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reserveTest(s, state, "alias", 10)
			if err == nil {
				admitted.Add(1)
				slotsMu.Lock()
				slots = append(slots, r.waitUntil)
				slotsMu.Unlock()
				r.reservation.finish(nil)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 16 {
		t.Fatalf("RPS burst should queue all slots inside 30s and reject only backlog, admitted %d", admitted.Load())
	}
	slotsMu.Lock()
	defer slotsMu.Unlock()
	if len(slots) != 16 {
		t.Fatalf("reserved slots: %d", len(slots))
	}
	for i := range slots {
		if slots[i].IsZero() {
			slots[i] = s.now()
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Before(slots[j]) })
	for i, slot := range slots {
		if slot.Sub(s.now()) != time.Duration(i)*2*time.Second {
			t.Fatalf("slot %d at %v", i, slot.Sub(s.now()))
		}
	}
}

func TestSubSecondRPSBurstsQueueInsteadOf429(t *testing.T) {
	s, now := rateTestState(t, "deepseek-v4-1-flash-1017")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var prepared []scheduledCompletion
	var rejected atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reserveTest(s, state, "alias", 10)
			if err != nil {
				rejected.Add(1)
				return
			}
			mu.Lock()
			prepared = append(prepared, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if rejected.Load() != 0 || len(prepared) != 20 {
		t.Fatalf("low-volume burst rejected %d of 20 requests", rejected.Load())
	}
	var offsets []time.Duration
	for _, r := range prepared {
		if !r.waitUntil.IsZero() {
			offsets = append(offsets, r.waitUntil.Sub(*now))
		}
		r.reservation.finish(nil)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	for i, offset := range offsets {
		if offset != time.Duration(i+1)*time.Duration(math.Ceil(float64(time.Second)/builtinRateLimits["deepseek-v4-1-flash-1017"].RPS)) {
			t.Fatalf("burst slot %d = %v", i, offset)
		}
	}
}

func TestHighRPSAllowsAccumulatedBurstImmediately(t *testing.T) {
	s, _ := rateTestState(t, "deepseek-v4-1-flash-1017")
	var admitted atomic.Int32
	var queued atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reserveTest(s, state, "alias", 10)
			if err != nil {
				t.Error(err)
				return
			}
			admitted.Add(1)
			if !r.waitUntil.IsZero() {
				queued.Add(1)
			}
			r.reservation.finish(nil)
		}()
	}
	wg.Wait()
	if admitted.Load() != 12 || queued.Load() != 0 {
		t.Fatalf("idle token bucket delayed a small burst: admitted=%d queued=%d", admitted.Load(), queued.Load())
	}
}

func TestIdleRPSBucketRefillsBeforeNextRequest(t *testing.T) {
	s, now := rateTestState(t, "glm-5-2")
	first, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal(err)
	}
	first.reservation.finish(nil)
	*now = now.Add(5 * time.Second)
	second, err := reserveTest(s, state, "alias", 10)
	if err != nil || !second.waitUntil.IsZero() {
		t.Fatalf("idle capacity not restored immediately: %+v %v", second, err)
	}
	second.reservation.finish(nil)
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

func TestRetryAfterParsing(t *testing.T) {
	_, now := rateTestState(t, "upstream")
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {"0.25", 250 * time.Millisecond}, {now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second}, {"NaN", 0}, {"+Inf", 0}, {"-1", 0}, {"bad", 0}, {"1e100", 0},
	} {
		if got := retryAfter(http.Header{"retry-after": {test.value}}, *now); got != test.want {
			t.Fatalf("%s: %v", test.value, got)
		}
	}
}

func TestCompletion429DelegatesCooldownToCPA(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "stream"}[streaming], func(t *testing.T) {
			rateTestState(t, "unknown-upstream")
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				if method == "host.http.do_stream" {
					return json.Marshal(upstreamStreamResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}})
				}
				return json.Marshal(upstreamResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}})
			}
			req := testNativeRequest(rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[]}`)})
			var err error
			if streaming {
				_, err = performCompletionStream(context.Background(), req, newRequestTrace(req))
			} else {
				_, _, err = performCompletion(context.Background(), req, newRequestTrace(req))
			}
			var detail *providerError
			if !errors.As(err, &detail) || detail.Code != "rate_limited" || detail.RetryAfterSeconds != 3 {
				t.Fatalf("lost native cooldown hint: %v", err)
			}
			// A reset or disable_cooling in CPA must not be overridden by hidden local state.
			r, err := reserveTest(completionLimits, state, "alias", 10)
			if err != nil {
				t.Fatal(err)
			}
			r.reservation.finish(nil)
		})
	}
}

func TestInvalidRequestDoesNotConsumeBudget(t *testing.T) {
	s, _ := rateTestState(t, "glm-5-2")
	req := rpcExecutorRequest{Model: "alias", Payload: []byte(`invalid`)}
	if _, err := s.prepare(state, testNativeRequest(req), &requestTrace{}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	r, err := reserveTest(s, state, "alias", 10)
	if err != nil {
		t.Fatal("invalid request consumed budget", err)
	}
	r.reservation.finish(nil)
}
