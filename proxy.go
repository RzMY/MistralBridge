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
	"sync"
	"time"
)

var proxyTransports = struct {
	sync.Mutex
	entries map[string]*http.Transport
	order   []string
}{}

func normalizeProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		return "direct", nil
	}
	invalid := errors.New("请输入有效的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理地址")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || strings.TrimSpace(parsed.Hostname()) == "" {
		return "", invalid
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", invalid
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", invalid
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
	} else if strings.HasSuffix(parsed.Host, ":") {
		return "", invalid
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func updateProxy(body []byte) ([]byte, error) {
	var input struct {
		ProxyURL *string `json:"proxy_url"`
	}
	if json.Unmarshal(body, &input) != nil || input.ProxyURL == nil {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "代理参数无效"})
	}
	proxyURL, err := normalizeProxyURL(*input.ProxyURL)
	if err != nil {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if err := mutateState(func(s *pluginState) error { s.ProxyURL = proxyURL; return nil }); err != nil {
		return mutationError(err)
	}
	closeProxyTransport()
	return managementJSON(publicState())
}

func closeProxyTransport() {
	proxyTransports.Lock()
	defer proxyTransports.Unlock()
	for _, transport := range proxyTransports.entries {
		transport.CloseIdleConnections()
	}
	proxyTransports.entries = nil
	proxyTransports.order = nil
}

func transportForProxy(proxyURL string) *http.Transport {
	proxyTransports.Lock()
	defer proxyTransports.Unlock()
	if existing := proxyTransports.entries[proxyURL]; existing != nil {
		return existing
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL != "direct" {
		parsed, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(parsed)
	}
	transport.ResponseHeaderTimeout = 2 * time.Minute
	if proxyTransports.entries == nil {
		proxyTransports.entries = make(map[string]*http.Transport)
	}
	// Bound retired proxy credentials while preserving active streams on eviction.
	if len(proxyTransports.order) >= 64 {
		oldest := proxyTransports.order[0]
		proxyTransports.order = proxyTransports.order[1:]
		proxyTransports.entries[oldest].CloseIdleConnections()
		delete(proxyTransports.entries, oldest)
	}
	proxyTransports.entries[proxyURL] = transport
	proxyTransports.order = append(proxyTransports.order, proxyURL)
	return transport
}

// CPA's HTTP callback does not accept a proxy override. Only explicit plugin
// proxies (including native credential proxies) use a local transport; an empty
// setting preserves CPA's global HTTP policy. Explicit direct bypasses env proxies.
func doUpstreamHTTP(ctx context.Context, request upstreamRequest, proxyURL string) (json.RawMessage, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return hostCall("host.http.do", request)
	}
	response, cleanup, err := proxyHTTPRequest(ctx, request, proxyURL)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(upstreamResponse{StatusCode: response.StatusCode, Headers: response.Header, Body: body})
}

func openUpstreamHTTPStream(ctx context.Context, request upstreamRequest, proxyURL string) (upstreamStreamResponse, error) {
	if strings.TrimSpace(proxyURL) == "" {
		raw, err := hostCall("host.http.do_stream", request)
		if err != nil {
			return upstreamStreamResponse{}, err
		}
		var stream upstreamStreamResponse
		if err := json.Unmarshal(raw, &stream); err != nil {
			return upstreamStreamResponse{}, err
		}
		return stream, nil
	}
	response, cleanup, err := proxyHTTPRequest(ctx, request, proxyURL)
	if err != nil {
		return upstreamStreamResponse{}, err
	}
	return upstreamStreamResponse{
		StatusCode: response.StatusCode, Headers: response.Header,
		reader: response.Body, cleanup: cleanup,
	}, nil
}

func proxyHTTPRequest(ctx context.Context, request upstreamRequest, rawProxy string) (*http.Response, func(), error) {
	proxyURL, err := normalizeProxyURL(rawProxy)
	if err != nil || proxyURL == "" {
		return nil, nil, errors.New("invalid plugin proxy configuration")
	}
	transport := transportForProxy(proxyURL)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	cleanup := func() { cancel() }
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	httpRequest.Header = request.Headers.Clone()
	client := &http.Client{Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		previous := via[len(via)-1]
		if next.URL.Scheme != previous.URL.Scheme || !strings.EqualFold(next.URL.Host, previous.URL.Host) {
			next.Header.Del("Authorization")
		}
		return nil
	}}
	response, err := client.Do(httpRequest)
	if err != nil {
		cleanup()
		return nil, nil, errors.New("upstream request through configured proxy failed")
	}
	return response, cleanup, nil
}

func (stream upstreamStreamResponse) opened() bool {
	return stream.reader != nil || stream.StreamID != ""
}

func (stream upstreamStreamResponse) read(callbackID string) (streamReadResponse, error) {
	if stream.reader == nil {
		raw, err := hostCall("host.http.stream_read", map[string]any{"stream_id": stream.StreamID, "host_callback_id": callbackID})
		if err != nil {
			return streamReadResponse{}, err
		}
		var chunk streamReadResponse
		err = json.Unmarshal(raw, &chunk)
		return chunk, err
	}
	buffer := make([]byte, 32*1024)
	n, err := stream.reader.Read(buffer)
	chunk := streamReadResponse{Payload: buffer[:n], Done: errors.Is(err, io.EOF)}
	if err != nil && !chunk.Done {
		chunk.Error = err.Error()
	}
	return chunk, nil
}

func (stream upstreamStreamResponse) close(callbackID string) {
	if stream.reader != nil {
		_ = stream.reader.Close()
		stream.cleanup()
		return
	}
	_, _ = hostCall("host.http.stream_close", map[string]any{"stream_id": stream.StreamID, "host_callback_id": callbackID})
}
