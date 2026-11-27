package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProxyPoolPersistenceValidationAndDeletion(t *testing.T) {
	isolatedState(t)
	response := managementCall(t, "POST", "/proxies", `{"name":"Singapore","url":"socks5://user:password@127.0.0.1:1080"}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	var entry proxyEntry
	_ = json.Unmarshal(response.Body, &entry)
	response = managementCall(t, "POST", "/keys", fmt.Sprintf(`{"label":"One","secret":"mock","proxy_mode":"pool","proxy_id":%q}`, entry.ID))
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	id := currentState().Keys[0].ID
	response = managementCall(t, "PATCH", "/keys", fmt.Sprintf(`{"id":%q,"label":"Rename","enabled":true}`, id))
	if response.StatusCode != 200 || currentState().Keys[0].ProxyID != entry.ID {
		t.Fatal("older client erased proxy binding")
	}
	stateLoaded = false
	loadState()
	if len(currentState().Proxies) != 1 || currentState().Keys[0].ProxyMode != "pool" {
		t.Fatal("proxy binding not persisted")
	}
	view := publicKey(currentState().Keys[0])
	if view.Mode != "pool" || view.keyProxyView.ID != entry.ID || strings.Contains(view.Address, "password") || strings.Contains(view.Address, "user") {
		t.Fatalf("proxy card leaked credentials: %+v", view.keyProxyView)
	}
	if managementCall(t, "DELETE", "/proxies", fmt.Sprintf(`{"id":%q}`, entry.ID)).StatusCode != 409 {
		t.Fatal("deleted bound proxy")
	}
	if managementCall(t, "PATCH", "/keys", fmt.Sprintf(`{"id":%q,"proxy_mode":"pool","proxy_id":"missing"}`, id)).StatusCode != 400 {
		t.Fatal("accepted missing proxy")
	}
	if managementCall(t, "PATCH", "/keys", fmt.Sprintf(`{"id":%q,"proxy_mode":"direct"}`, id)).StatusCode != 200 {
		t.Fatal("direct failed")
	}
	if managementCall(t, "DELETE", "/proxies", fmt.Sprintf(`{"id":%q}`, entry.ID)).StatusCode != 200 {
		t.Fatal("unbound proxy not deleted")
	}
	path, _ := statePath()
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if managementCall(t, "POST", "/proxies", `{"name":"Fail","url":"http://127.0.0.1:7890"}`).StatusCode != 500 || len(currentState().Proxies) != 0 {
		t.Fatal("failed save did not roll back")
	}
}

func TestNativeProxyOwnershipAndPrecedence(t *testing.T) {
	isolatedState(t)
	settings := pluginState{ProxyURL: "http://default:80", Proxies: []proxyEntry{{ID: "a", URL: "http://pool:80"}}}
	key := apiKey{ID: "one", ProxyMode: "pool", ProxyID: "a"}
	doc := map[string]any{"weight": float64(3)}
	changed, err := syncNativeKeyProxy(doc, key, settings, apiKey{}, false, false)
	if err != nil || !changed || doc["proxy_url"] != "http://pool:80" {
		t.Fatal("pool not exported", doc, err)
	}
	settings.Proxies[0].URL = "http://pool-new:80"
	_, err = syncNativeKeyProxy(doc, key, settings, key, true, true)
	if err != nil || doc["proxy_url"] != "http://pool-new:80" {
		t.Fatal("pool update not synchronized")
	}
	doc["proxy_url"] = "http://cpa:80"
	settings.Proxies[0].URL = "http://pool-next:80"
	changed, err = syncNativeKeyProxy(doc, key, settings, key, true, true)
	if err != nil || changed || doc["proxy_url"] != "http://cpa:80" || doc["weight"] != float64(3) {
		t.Fatal("native override overwritten")
	}
	nativeMetadata.Store(key.ID, doc)
	got, err := credentialProxyURL(settings, key)
	if err != nil || got != "http://cpa:80" || publicKeyProxy(settings, key).Mode != "native" {
		t.Fatal("native override not honored", got, err)
	}
	doc["proxy_url"] = "none"
	if got, _ := credentialProxyURL(settings, key); got != "direct" {
		t.Fatal("native direct ignored")
	}
	doc["proxy_url"] = ""
	if got, _ := credentialProxyURL(settings, key); got != settings.ProxyURL {
		t.Fatal("native clear did not inherit default")
	}
	old := key
	old.ProxyMode = "explicit-edit"
	_, err = syncNativeKeyProxy(doc, key, settings, old, true, true)
	if err != nil || doc["proxy_url"] != "http://pool-next:80" {
		t.Fatal("explicit rebind failed")
	}
	old = key
	key.ProxyMode = "inherit"
	key.ProxyID = ""
	_, err = syncNativeKeyProxy(doc, key, settings, old, true, true)
	if err != nil || authString(doc, "proxy_url") != "" || authString(doc, "bridge_proxy_mode") != "" {
		t.Fatal("unbind left native proxy")
	}
	doc["proxy_url"] = "http://other:80"
	changed, err = syncNativeKeyProxy(doc, key, settings, apiKey{}, false, true)
	if err != nil || changed {
		t.Fatal("restart claimed native proxy")
	}
	requestProxy := "http://snapshot:80"
	key.NativeProxyURL = &requestProxy
	if got, _ := credentialProxyURL(settings, key); got != requestProxy {
		t.Fatal("request snapshot not honored")
	}
}

func TestPerCredentialProxyCompletionCatalogAndDirect(t *testing.T) {
	isolatedState(t)
	t.Cleanup(closeProxyTransport)
	var hits [2]atomic.Int32
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		index := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[index].Add(1)
			if r.Header.Get("Authorization") != fmt.Sprintf("Bearer mock-%d", index) {
				t.Error("credential used another proxy")
			}
			if r.URL.Path == "/v1/models" {
				io.WriteString(w, `{"data":[{"id":"mock"}]}`)
				return
			}
			io.WriteString(w, `{"choices":[],"usage":{"total_tokens":1}}`)
		}))
		defer servers[i].Close()
	}
	state.Keys = []apiKey{{ID: "a", Secret: "mock-0", Enabled: true, BaseURL: "http://mistral.invalid/v1"}, {ID: "b", Secret: "mock-1", Enabled: true, BaseURL: "http://mistral.invalid/v1"}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "unknown", Enabled: true}}
	state.ProxyURL = "http://127.0.0.1:1"
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		t.Errorf("unexpected callback %s", method)
		return nil, fmt.Errorf("unexpected callback")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		index := i % 2
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := state.Keys[index]
			req := rpcExecutorRequest{AuthID: key.ID, AuthProvider: pluginID, AuthMetadata: map[string]any{"bridge_key_id": key.ID, "proxy_url": servers[index].URL}, Model: "alias", Payload: []byte(`{"messages":[]}`)}
			if _, _, err := performCompletion(context.Background(), req, newRequestTrace(req)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i, key := range state.Keys {
		nativeMetadata.Store(key.ID, map[string]any{"proxy_url": servers[i].URL})
		if _, err := fetchModels(key, ""); err != nil {
			t.Fatal(err)
		}
		if hits[i].Load() != 11 {
			t.Fatalf("proxy %d handled %d", i, hits[i].Load())
		}
	}
	if transportForProxy(servers[0].URL) != transportForProxy(servers[0].URL) || transportForProxy(servers[0].URL) == transportForProxy(servers[1].URL) {
		t.Fatal("transports not isolated and reused")
	}
	if transportForProxy("direct").Proxy != nil {
		t.Fatal("direct inherited environment proxy")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[{"id":"direct"}]}`) }))
	defer upstream.Close()
	key := state.Keys[0]
	key.BaseURL = upstream.URL
	nativeMetadata.Store(key.ID, map[string]any{"proxy_url": "direct"})
	if models, err := fetchModels(key, ""); err != nil || len(models) != 1 || models[0] != "direct" {
		t.Fatal("direct did not bypass default", err)
	}
}
