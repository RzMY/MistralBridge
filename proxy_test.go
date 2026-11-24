package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxySettingsValidationPersistenceAndRollback(t *testing.T) {
	isolatedState(t)
	t.Cleanup(closeProxyTransport)
	for _, raw := range []string{
		"http://127.0.0.1:7890", "https://user:p%40ss@proxy.example:443/",
		"socks5://[::1]:1080", "socks5h://user:password@proxy.example", "http://proxy.example",
	} {
		if _, err := normalizeProxyURL(raw); err != nil {
			t.Fatalf("valid proxy %q rejected: %v", raw, err)
		}
	}
	for _, body := range []string{
		`{}`, `{"proxy_url":null}`, `{"proxy_url":1}`, `invalid`,
		`{"proxy_url":"127.0.0.1:7890"}`, `{"proxy_url":"ftp://proxy.example:21"}`,
		`{"proxy_url":"http://:7890"}`, `{"proxy_url":"http://proxy.example:0"}`,
		`{"proxy_url":"http://proxy.example:65536"}`, `{"proxy_url":"http://proxy.example:abc"}`,
		`{"proxy_url":"http://proxy.example:"}`, `{"proxy_url":"http://proxy.example/path"}`,
		`{"proxy_url":"http://proxy.example?token=secret"}`, `{"proxy_url":"http://proxy.example#fragment"}`,
	} {
		response := managementCall(t, "PATCH", "/proxy", body)
		if response.StatusCode != http.StatusBadRequest || currentState().ProxyURL != "" {
			t.Fatalf("invalid proxy accepted: %s, response %s", body, response.Body)
		}
	}
	revision := publicState()["models_revision"]
	response := managementCall(t, "PATCH", "/proxy", `{"proxy_url":"  http://user:password@127.0.0.1:7890/  "}`)
	want := "http://user:password@127.0.0.1:7890"
	if response.StatusCode != http.StatusOK || currentState().ProxyURL != want || publicState()["models_revision"] != revision {
		t.Fatalf("saving proxy changed unrelated state: %s", response.Body)
	}
	stateLoaded = false
	loadState()
	if currentState().ProxyURL != want || publicState()["proxy_url"] != want {
		t.Fatal("proxy was not persisted or exposed to the settings page")
	}
	response = managementCall(t, "PATCH", "/proxy", `{"proxy_url":"  "}`)
	stateLoaded = false
	loadState()
	if response.StatusCode != http.StatusOK || currentState().ProxyURL != "" {
		t.Fatal("clearing proxy was not persisted")
	}
	path, _ := statePath()
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	response = managementCall(t, "PATCH", "/proxy", `{"proxy_url":"socks5://127.0.0.1:1080"}`)
	if response.StatusCode != http.StatusInternalServerError || currentState().ProxyURL != "" {
		t.Fatal("failed proxy save changed the active configuration")
	}
}

func TestIndependentHTTPProxyForCompletionAndCatalog(t *testing.T) {
	isolatedState(t)
	t.Cleanup(closeProxyTransport)
	var requests int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNzd29yZA==" || r.Header.Get("Authorization") != "Bearer upstream-key" || r.URL.Host != "mistral.invalid" {
			t.Errorf("proxy received unexpected credentials or destination: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"chat-model","capabilities":{"completion_chat":true}}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"upstream"`) || !strings.Contains(string(body), `"stream":false`) {
			t.Errorf("completion was not transformed: %s", body)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer proxy.Close()
	state.ProxyURL = strings.Replace(proxy.URL, "http://", "http://user:password@", 1)
	state.Keys = []apiKey{{ID: "key", Secret: "upstream-key", BaseURL: "http://mistral.invalid/v1", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		return nil, fmt.Errorf("independent proxy unexpectedly used host callback %s", method)
	}
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[]}`)})
	if response, err := execute(raw); err != nil || !strings.Contains(string(response), "ok") {
		t.Fatalf("completion failed: %s, %v", response, err)
	}
	if response := managementCall(t, "POST", "/keys/test", `{"id":"key"}`); response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	if response := managementCall(t, "POST", "/models/sync", `{"key_id":"key"}`); response.StatusCode != http.StatusOK || len(currentState().Catalog) != 1 {
		t.Fatal(string(response.Body))
	}
	if requests != 3 {
		t.Fatalf("proxy handled %d requests, want 3", requests)
	}
	if response := managementCall(t, "PATCH", "/proxy", `{"proxy_url":""}`); response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	var callbacks int
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		request := payload.(upstreamRequest)
		if method != "host.http.do" || request.HostCallbackID != "cpa-context" {
			t.Fatal("cleared proxy did not restore the CPA callback context")
		}
		callbacks++
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"host-model"}]}`)})
	}
	if _, err := fetchModels(currentState().Keys[0], "cpa-context"); err != nil || callbacks != 1 || requests != 3 {
		t.Fatal("cleared configuration did not fall back to CPA")
	}
}

func TestIndependentProxyFailureDoesNotFallBack(t *testing.T) {
	t.Cleanup(closeProxyTransport)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + listener.Addr().String()
	listener.Close()
	original := hostCall
	t.Cleanup(func() { hostCall = original })
	called := false
	hostCall = func(string, any) (json.RawMessage, error) { called = true; return nil, nil }
	request := upstreamRequest{Method: "GET", URL: "http://mistral.invalid/v1/models"}
	if _, err := doUpstreamHTTP(context.Background(), request, proxyURL); err == nil || called {
		t.Fatal("failed plugin proxy silently fell back to CPA")
	}
	if _, err := openUpstreamHTTPStream(context.Background(), request, proxyURL); err == nil || called {
		t.Fatal("failed plugin streaming proxy silently fell back to CPA")
	}
}

func TestIndependentProxyRedactsAuthorizationOnRedirect(t *testing.T) {
	t.Cleanup(closeProxyTransport)
	var targetAuthorization string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "mistral.invalid" {
			if r.Header.Get("Authorization") != "Bearer private-key" {
				t.Error("initial upstream authorization missing")
			}
			http.Redirect(w, r, "http://other.invalid/redirected", http.StatusFound)
			return
		}
		targetAuthorization = r.Header.Get("Authorization")
		io.WriteString(w, "ok")
	}))
	defer proxy.Close()
	_, err := doUpstreamHTTP(context.Background(), upstreamRequest{Method: "GET", URL: "http://mistral.invalid/models", Headers: http.Header{"Authorization": {"Bearer private-key"}}}, proxy.URL)
	if err != nil || targetAuthorization != "" {
		t.Fatalf("redirect leaked upstream authorization: %q, %v", targetAuthorization, err)
	}
}

func TestIndependentProxyStreamsAndErrorBodies(t *testing.T) {
	isolatedState(t)
	t.Cleanup(closeProxyTransport)
	state.DebugMode = true
	state.Keys = []apiKey{{ID: "key", Secret: "secret", BaseURL: "http://mistral.invalid/v1", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", Enabled: true}}
	firstChunk := make(chan struct{})
	continueStream := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream_options"`) {
			t.Errorf("native Mistral stream must omit OpenAI stream_options: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"thinking\",\"thinking\":[{\"type\":\"text\",\"text\":\"thought\"}]}]}}]}\n\n")
		w.(http.Flusher).Flush()
		close(firstChunk)
		<-continueStream
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer proxy.Close()
	state.ProxyURL = proxy.URL
	var mu sync.Mutex
	var emitted []byte
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.stream.emit":
			mu.Lock()
			emitted = append(emitted, payload.(map[string]any)["payload"].([]byte)...)
			mu.Unlock()
		case "host.stream.close":
		default:
			return nil, fmt.Errorf("unexpected CPA HTTP callback: %s", method)
		}
		return json.Marshal(map[string]bool{"ok": true})
	}
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "alias", StreamID: "client", HostCallbackID: "callback", Payload: []byte(`{"messages":[]}`)})
	_, err := executeStream(raw)
	if err != nil {
		close(continueStream)
		t.Fatal(err)
	}
	<-firstChunk
	// Clearing configuration must leave an already-open local stream intact.
	response := managementCall(t, "PATCH", "/proxy", `{"proxy_url":""}`)
	close(continueStream)
	if response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	var entry RequestLog
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stateMu.RLock()
		if len(state.Logs) > 0 {
			entry = state.Logs[0]
		}
		stateMu.RUnlock()
		if entry.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	actual := string(emitted)
	mu.Unlock()
	if entry.Status != "ok" || entry.TotalTokens != 5 || !entry.DebugAvailable || !strings.Contains(actual, `"reasoning_content":"thought"`) || !strings.Contains(actual, "[DONE]") {
		t.Fatalf("stream result: log %+v, events %s", entry, actual)
	}
	path, _ := debugLogPath(entry.ID)
	stored, _ := os.ReadFile(path)
	var exchange debugExchange
	if json.Unmarshal(stored, &exchange) != nil || !strings.Contains(exchange.Response.Body, "answer") || !exchange.Response.Complete {
		t.Fatalf("local stream debug capture incomplete: %s", stored)
	}
	failedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":{"message":"unsupported reasoning_effort"}}`)
	}))
	defer failedProxy.Close()
	stateMu.Lock()
	state.ProxyURL = failedProxy.URL
	stateMu.Unlock()
	_, err = executeStream(raw)
	var status *statusError
	if !errors.As(err, &status) || status.Status != 422 || status.Message != "unsupported reasoning_effort" {
		t.Fatalf("proxy stream lost upstream error: %v", err)
	}
}

func TestIndependentHTTPSProxyAndQuotaTunnel(t *testing.T) {
	isolatedState(t)
	t.Cleanup(closeProxyTransport)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer quota-key" {
			t.Error("quota authorization missing")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "budget"):
			io.WriteString(w, `{"api_budget":{"initial_budget":50,"usage_percentage":40,"currency":"EUR"}}`)
		case strings.HasSuffix(r.URL.Path, "subscription"):
			io.WriteString(w, `{"plan_name":"INDIVIDUAL"}`)
		default:
			io.WriteString(w, `{"plan_type":"api","plan_name":"INDIVIDUAL"}`)
		}
	}))
	defer upstream.Close()
	originalTransport := http.DefaultTransport
	testTransport := upstream.Client().Transport.(*http.Transport).Clone()
	// Both ends of the loopback tunnel use httptest's trusted localhost certificate.
	testTransport.TLSClientConfig.ServerName = "127.0.0.1"
	http.DefaultTransport = testTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var tunnels int
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "api.mistral.ai:443" && r.Host != "console.mistral.ai:443") {
			t.Errorf("unexpected tunnel: %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		tunnels++
		remote, err := net.Dial("tcp", upstream.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			t.Error(err)
			return
		}
		defer conn.Close()
		defer remote.Close()
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(remote, conn)
		io.Copy(conn, remote)
	}))
	defer proxy.Close()
	state.ProxyURL = proxy.URL
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		return nil, fmt.Errorf("quota unexpectedly used CPA HTTP callback: %s", method)
	}
	quota, err := fetchQuota(apiKey{Secret: "quota-key"}, "callback")
	if err != nil || quota.Limit != 50 || quota.Remaining != 30 || tunnels == 0 {
		t.Fatalf("quota through HTTPS proxy = %+v, %v; tunnels %d", quota, err, tunnels)
	}
}

func TestIndependentSOCKSProxyAuthenticationAndRemoteDNS(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			t.Cleanup(closeProxyTransport)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			result := make(chan error, 1)
			go func() { result <- serveTestSOCKS(listener) }()
			raw, err := doUpstreamHTTP(context.Background(), upstreamRequest{Method: "GET", URL: "http://mistral.invalid/v1/models"}, scheme+"://user:password@"+listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			var response upstreamResponse
			if json.Unmarshal(raw, &response) != nil || response.StatusCode != 200 || string(response.Body) != "proxied" {
				t.Fatalf("SOCKS response = %s", raw)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func serveTestSOCKS(listener net.Listener) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	read := func(size int) ([]byte, error) {
		buffer := make([]byte, size)
		_, err := io.ReadFull(conn, buffer)
		return buffer, err
	}
	header, err := read(2)
	if err != nil || header[0] != 5 {
		return fmt.Errorf("invalid SOCKS greeting: %v", err)
	}
	if _, err := read(int(header[1])); err != nil {
		return err
	}
	conn.Write([]byte{5, 2})
	header, err = read(2)
	if err != nil || header[0] != 1 {
		return fmt.Errorf("invalid SOCKS auth greeting: %v", err)
	}
	username, err := read(int(header[1]))
	if err != nil {
		return err
	}
	length, err := read(1)
	if err != nil {
		return err
	}
	password, err := read(int(length[0]))
	if err != nil || string(username) != "user" || string(password) != "password" {
		return fmt.Errorf("SOCKS credentials missing: %v", err)
	}
	conn.Write([]byte{1, 0})
	header, err = read(5)
	if err != nil || header[0] != 5 || header[1] != 1 || header[3] != 3 {
		return fmt.Errorf("SOCKS request did not use remote DNS: %v", err)
	}
	destination, err := read(int(header[4]) + 2)
	if err != nil || string(destination[:len(destination)-2]) != "mistral.invalid" {
		return fmt.Errorf("invalid SOCKS destination: %v", err)
	}
	conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return err
	}
	defer request.Body.Close()
	_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nproxied")
	return err
}
