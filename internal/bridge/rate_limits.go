package bridge

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot of docs/Rate limits - Admin - Mistral.html (EU, 2026-11-25).
// These are the downloaded account's limits, not universal Mistral plan limits.
// Match upstream IDs exactly: a moving -latest alias must not be guessed.
//
//go:embed data/rate_limits.json
var rateLimitsJSON []byte

type modelRateLimit struct {
	TPM int     `json:"tpm"`
	RPS float64 `json:"rps"`
}

var builtinRateLimits = func() map[string]modelRateLimit {
	var limits map[string]modelRateLimit
	if err := json.Unmarshal(rateLimitsJSON, &limits); err != nil {
		panic(err)
	}
	return limits
}()

type rateBucket struct {
	rpsTokens    float64
	rpsUpdatedAt time.Time
	tokens       []*tokenReservation
}

type tokenReservation struct {
	scheduler *completionScheduler
	bucket    *rateBucket
	tokens    int
	expires   time.Time
	done      bool
}

type completionScheduler struct {
	mu      sync.Mutex
	buckets map[[32]byte]*rateBucket
	now     func() time.Time
}

func newCompletionScheduler() *completionScheduler {
	return &completionScheduler{buckets: make(map[[32]byte]*rateBucket), now: time.Now}
}

var completionLimits = newCompletionScheduler()

// RPS uses a token bucket, not a fixed interval. An idle credential starts
// with a full burst allowance, so one request at 0.5 RPS is never delayed.
const maxRPSQueueWait = 30 * time.Second

type scheduledCompletion struct {
	key         apiKey
	model       modelMapping
	body        []byte
	headers     http.Header
	endpoint    string
	proxyURL    string
	reservation *tokenReservation
	waitUntil   time.Time
}

func rateScope(key apiKey, model string) [32]byte {
	base := strings.TrimRight(key.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	// Client aliases and channel changes share the same upstream budget. Never
	// retain a raw credential in the scheduler or expose it in diagnostic state.
	return sha256.Sum256([]byte(base + "\x00" + key.Secret + "\x00" + model))
}

func (s *completionScheduler) prepare(settings pluginState, req rpcExecutorRequest, trace *requestTrace) (scheduledCompletion, error) {
	var model modelMapping
	for _, candidate := range settings.Models {
		if candidate.Enabled && candidate.ClientModel == strings.TrimPrefix(req.Model, pluginID+"/") {
			model = candidate
			break
		}
	}
	if model.ClientModel == "" {
		return scheduledCompletion{}, &statusError{404, "Mistral model mapping was not found"}
	}
	key, err := selectedNativeKey(req)
	if err != nil {
		return scheduledCompletion{}, err
	}
	if model.KeyID != "" && model.KeyID != key.ID {
		return scheduledCompletion{}, &statusError{404, "Selected CPA credential is not bound to this model"}
	}
	proxyURL, err := credentialProxyURL(settings, key)
	if err != nil {
		return scheduledCompletion{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	trace.resolved(key, model, req.Payload)
	body, headers, drops, err := prepareCompletionRequest(req, key, model, settings.RequestEdit)
	trace.recordDroppedFields(drops)
	if err != nil {
		return scheduledCompletion{}, err
	}
	endpoint, err := completionURL(key.BaseURL)
	if err != nil {
		return scheduledCompletion{}, &statusError{500, "Invalid Mistral API endpoint"}
	}
	limit := builtinRateLimits[model.UpstreamModel]
	estimate := estimateRequestTokens(body)
	if limit.TPM > 0 {
		estimate = min(estimate, limit.TPM)
	}
	scope := rateScope(key, model.UpstreamModel)
	bucket := s.buckets[scope]
	if bucket == nil {
		bucket = &rateBucket{}
		s.buckets[scope] = bucket
	}
	// TPM is a rolling minute budget. RPS is a token bucket: brief concurrent
	// bursts use accumulated capacity, and only requests exceeding it are paced.
	tokenAvailable := bucket.tokenAvailableAt(now, limit, estimate)
	if tokenAvailable.After(now) {
		return scheduledCompletion{}, cooldownError(tokenAvailable.Sub(now))
	}
	sendAt, remainingRPS, rpsUpdatedAt := bucket.reserveRPS(now, limit)
	if sendAt.Sub(now) > maxRPSQueueWait {
		return scheduledCompletion{}, cooldownError(sendAt.Sub(now))
	}
	if limit.RPS > 0 {
		bucket.rpsTokens, bucket.rpsUpdatedAt = remainingRPS, rpsUpdatedAt
	}
	reservation := &tokenReservation{scheduler: s, bucket: bucket, tokens: estimate}
	if limit.TPM > 0 {
		bucket.tokens = append(bucket.tokens, reservation)
	}
	trace.resolved(key, model, body)
	var waitUntil time.Time
	if sendAt.After(now) {
		waitUntil = sendAt
	}
	return scheduledCompletion{key, model, body, headers, endpoint, proxyURL, reservation, waitUntil}, nil
}

func (b *rateBucket) tokenAvailableAt(now time.Time, limit modelRateLimit, estimate int) time.Time {
	var until time.Time
	active := b.tokens[:0]
	total := 0
	for _, r := range b.tokens {
		if !r.done || r.expires.After(now) {
			active = append(active, r)
			total += r.tokens
		}
	}
	b.tokens = active
	if limit.TPM <= 0 || total+estimate <= limit.TPM {
		return until
	}
	// Keep in-flight reservations until completion, even for streams over a
	// minute. Settled usage expires a minute after completion (conservative).
	expiring := append([]*tokenReservation(nil), active...)
	sort.Slice(expiring, func(i, j int) bool { return expiring[i].expires.Before(expiring[j].expires) })
	for _, r := range expiring {
		if !r.done {
			continue
		}
		total -= r.tokens
		if r.expires.After(until) {
			until = r.expires
		}
		if total+estimate <= limit.TPM {
			return until
		}
	}
	if retry := now.Add(time.Minute); retry.After(until) {
		until = retry
	}
	return until
}

func (b *rateBucket) reserveRPS(now time.Time, limit modelRateLimit) (time.Time, float64, time.Time) {
	if limit.RPS <= 0 {
		return now, b.rpsTokens, b.rpsUpdatedAt
	}
	capacity := math.Max(1, math.Floor(limit.RPS))
	tokens, updatedAt := b.rpsTokens, b.rpsUpdatedAt
	if updatedAt.IsZero() {
		tokens, updatedAt = capacity, now
	} else if now.After(updatedAt) {
		tokens = math.Min(capacity, tokens+now.Sub(updatedAt).Seconds()*limit.RPS)
		updatedAt = now
	}
	if tokens >= 1 {
		return now, tokens - 1, updatedAt
	}
	base := updatedAt
	if base.Before(now) {
		base = now
	}
	wait := time.Duration(math.Ceil((1 - tokens) / limit.RPS * float64(time.Second)))
	sendAt := base.Add(wait)
	// Reserve a future token slot. Concurrent arrivals are scheduled after it.
	return sendAt, 0, sendAt
}

// No tokenizer is available on CPA's completion route. Reserve an approximate
// input budget plus max_tokens, then reconcile against upstream usage. Missing
// usage keeps the estimate; missing max_tokens reserves 4096 output tokens.
func estimateRequestTokens(body []byte) int {
	var request struct {
		Messages  json.RawMessage `json:"messages"`
		Tools     json.RawMessage `json:"tools"`
		MaxTokens int             `json:"max_tokens"`
	}
	_ = json.Unmarshal(body, &request)
	output := request.MaxTokens
	if output <= 0 {
		output = 4096
	}
	input := (len(request.Messages)+len(request.Tools)+2)/3 + 16
	// Avoid integer overflow from untrusted max_tokens.
	if output > math.MaxInt-input {
		return math.MaxInt
	}
	return input + output
}

func (r *tokenReservation) finish(usage map[string]int) {
	if r == nil {
		return
	}
	s := r.scheduler
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	r.expires = s.now().Add(time.Minute)
	if _, totalKnown := usage["total_tokens"]; totalKnown || usage["prompt_tokens"] > 0 || usage["completion_tokens"] > 0 {
		r.tokens = max(usage["total_tokens"], usage["prompt_tokens"]+usage["completion_tokens"])
	}
}

func cooldownError(wait time.Duration) *providerError {
	seconds := max(1, int(math.Ceil(wait.Seconds())))
	return &providerError{statusError: &statusError{429, fmt.Sprintf("Mistral model rate limit; retry after %d seconds", seconds)}, Code: "transient_rate_limit", RetryAfterSeconds: seconds}
}

func retryAfter(headers http.Header, now time.Time) time.Duration {
	for name, values := range headers {
		if !strings.EqualFold(name, "Retry-After") {
			continue
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 && !math.IsInf(seconds, 0) && seconds < float64(math.MaxInt64)/float64(time.Second) {
				return time.Duration(math.Ceil(seconds * float64(time.Second)))
			}
			if date, err := http.ParseTime(value); err == nil && date.After(now) {
				return date.Sub(now)
			}
		}
	}
	return 0
}
