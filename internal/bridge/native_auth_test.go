package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testNativeRequest(req rpcExecutorRequest) rpcExecutorRequest {
	if req.AuthID != "" || len(state.Keys) == 0 {
		return req
	}
	req.AuthID = nativeAuthFile(state.Keys[0].ID)
	req.AuthProvider = pluginID
	req.AuthMetadata = map[string]any{"type": pluginID, "bridge_key_id": state.Keys[0].ID}
	return req
}

func testNativeRaw(raw []byte) []byte {
	var req rpcExecutorRequest
	_ = json.Unmarshal(raw, &req)
	raw, _ = json.Marshal(testNativeRequest(req))
	return raw
}

func TestNativeSelectionAndBoundModels(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "one", Secret: "mock-one", Enabled: true, Priority: 100}, {ID: "two", Secret: "mock-two", Enabled: true, Priority: 0}}
	state.Models = []modelMapping{{ClientModel: "shared", UpstreamModel: "upstream", Enabled: true}, {ClientModel: "bound", UpstreamModel: "upstream", Enabled: true, KeyID: "one"}}
	req := rpcExecutorRequest{AuthID: "cpa-second", AuthProvider: pluginID, AuthMetadata: map[string]any{"bridge_key_id": "two"}, Model: "shared", Payload: []byte(`{"messages":[]}`)}
	r, err := completionLimits.prepare(state, req, &requestTrace{})
	if err != nil || r.key.ID != "two" {
		t.Fatalf("ignored CPA selection: %+v %v", r.key, err)
	}
	r.reservation.finish(nil)
	req.Model = "bound"
	if _, err := completionLimits.prepare(state, req, &requestTrace{}); err == nil {
		t.Fatal("bound model used another key")
	}
	raw, _ := json.Marshal(nativeAuthRequest{AuthID: req.AuthID, AuthProvider: pluginID, Metadata: req.AuthMetadata})
	reply, err := nativeModels(raw)
	if err != nil || !strings.Contains(string(reply), `"ID":"shared"`) || strings.Contains(string(reply), `"ID":"bound"`) {
		t.Fatalf("native eligibility %s %v", reply, err)
	}
	req.AuthID = ""
	if _, err := selectedNativeKey(req); err == nil {
		t.Fatal("missing native selection fell back")
	}
	req.AuthID = "cpa-second"
	req.AuthMetadata["bridge_key_id"] = "deleted"
	if _, err := selectedNativeKey(req); err == nil {
		t.Fatal("deleted selection fell back")
	}
}

func TestNativeParsePreservesPolicyAndStandaloneKey(t *testing.T) {
	isolatedState(t)
	doc := map[string]any{"type": pluginID, "api_key": "mock-direct", "mistral_base_url": "http://127.0.0.1:1234/v1", "priority": 7, "weight": 3, "disable_cooling": true, "request_retry": 0, "prefix": "test", "proxy_url": "direct", "label": "Direct", "unknown_option": "preserve"}
	storage, _ := json.Marshal(doc)
	raw, _ := json.Marshal(nativeAuthRequest{Provider: pluginID, FileName: "native.json", RawJSON: storage})
	reply, err := parseNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(reply, &env)
	var result struct {
		Handled bool
		Auth    nativeAuthData
	}
	_ = json.Unmarshal(env.Result, &result)
	if !result.Handled || result.Auth.Attributes["weight"] != "3" || result.Auth.Attributes["priority"] != "7" || result.Auth.Prefix != "test" || result.Auth.ProxyURL != "direct" || result.Auth.Metadata["unknown_option"] != "preserve" {
		t.Fatalf("lost native fields: %+v", result)
	}
	if _, ok := result.Auth.Attributes["base_url"]; ok {
		t.Fatal("would select native compat executor")
	}
	if result.Auth.Label != "Direct" || result.Auth.Attributes["auth_kind"] != "apikey" || result.Auth.Attributes["api_key"] != "" {
		t.Fatalf("credential display name is shadowed by an account marker: %+v", result.Auth)
	}
	key, err := selectedNativeKey(rpcExecutorRequest{AuthID: "native.json", AuthProvider: pluginID, StorageJSON: storage})
	if err != nil || key.Secret != "mock-direct" || key.BaseURL != doc["mistral_base_url"] {
		t.Fatalf("standalone key %+v %v", key, err)
	}
}

func TestNativeManagedNameAndLegacyRefresh(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "one", Label: "Studio2", Enabled: true, Secret: "not-a-display-name"}}
	storage := []byte(`{"type":"mistral-bridge","bridge_key_id":"one","label":"Studio2"}`)
	raw, _ := json.Marshal(nativeAuthRequest{Provider: pluginID, FileName: nativeAuthFile("one"), RawJSON: storage})
	reply, err := parseNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(reply, &env)
	var result struct{ Auth nativeAuthData }
	_ = json.Unmarshal(env.Result, &result)
	if result.Auth.Label != "Studio2" || result.Auth.Attributes["api_key"] != "" || result.Auth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("managed credential identity: %+v", result.Auth)
	}
	raw, _ = json.Marshal(nativeAuthRequest{AuthID: nativeAuthFile("one"), StorageJSON: storage, Attributes: map[string]string{"api_key": "plugin-managed", "weight": "3"}})
	reply, err = refreshNativeAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(reply, &env)
	_ = json.Unmarshal(env.Result, &result)
	marker, present := result.Auth.Attributes["api_key"]
	if !present || marker != "" || result.Auth.Attributes["weight"] != "3" || result.Auth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("legacy marker survives refresh: %+v", result.Auth.Attributes)
	}
}

func TestNativeMigrationSyncAndRemoval(t *testing.T) {
	isolatedState(t)
	nativeAuthEnabled.Store(true)
	t.Cleanup(func() { nativeAuthEnabled.Store(false) })
	state.Keys = []apiKey{{ID: "one", Label: "One", Secret: "must-not-export", Priority: 7, Enabled: true}}
	files := map[string]map[string]any{}
	dir := t.TempDir()
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		encoded, _ := json.Marshal(payload)
		var input map[string]json.RawMessage
		_ = json.Unmarshal(encoded, &input)
		switch method {
		case "host.auth.list":
			rows := []map[string]string{}
			for name := range files {
				rows = append(rows, map[string]string{"name": name, "auth_index": name})
			}
			return json.Marshal(map[string]any{"files": rows})
		case "host.auth.get":
			var index string
			_ = json.Unmarshal(input["auth_index"], &index)
			return json.Marshal(map[string]any{"json": files[index]})
		case "host.auth.save":
			var name string
			_ = json.Unmarshal(input["name"], &name)
			var doc map[string]any
			_ = json.Unmarshal(input["json"], &doc)
			files[name] = doc
			if strings.Contains(string(input["json"]), "must-not-export") {
				t.Fatal("copied private key into native reference")
			}
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, input["json"], 0600); err != nil {
				t.Fatal(err)
			}
			return json.Marshal(map[string]string{"path": path})
		}
		return nil, errors.New("unexpected host call")
	}
	if err := syncNativeAuths(nil, true); err != nil {
		t.Fatal(err)
	}
	name := nativeAuthFile("one")
	files[name]["weight"] = float64(5)
	files[name]["disable_cooling"] = true
	files[name]["priority"] = float64(99)
	before := currentState()
	state.Keys[0].Label = "Renamed"
	if err := syncNativeAuths(&before, false); err != nil {
		t.Fatal(err)
	}
	if files[name]["weight"] != float64(5) || files[name]["disable_cooling"] != true || files[name]["priority"] != float64(99) || files[name]["label"] != "Renamed" {
		t.Fatalf("clobbered CPA settings: %+v", files[name])
	}
	before = currentState()
	state.Keys[0].Priority = 10
	if err := syncNativeAuths(&before, false); err != nil {
		t.Fatal(err)
	}
	if files[name]["priority"] != float64(10) {
		t.Fatal("explicit priority update missing")
	}
	files[name]["disabled"] = true
	nativeMetadata.Store("one", files[name])
	if response := managementCall(t, "PATCH", "/keys/toggle", `{"id":"one"}`); response.StatusCode != 200 || files[name]["disabled"] != false {
		t.Fatal("implicit toggle did not use native disabled state")
	}
	before = currentState()
	state.Keys = nil
	if err := syncNativeAuths(&before, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) || files[name]["disabled"] != true {
		t.Fatal("orphan not disabled and removed")
	}
}

func TestNativeErrorEnvelope(t *testing.T) {
	for _, test := range []struct {
		err          error
		code         string
		status, wait int
	}{
		{cooldownError(2e9), "transient_rate_limit", 429, 2},
		{nativeUpstreamError(429, map[string][]string{"Retry-After": {"17"}}, []byte(`{"message":"limited"}`)), "rate_limited", 429, 17},
		{nativeUpstreamError(429, nil, nil), "rate_limited", 429, 0},
		{&statusError{400, "invalid"}, "request_scoped", 400, 0},
		{&statusError{501, "unsupported"}, "not_implemented", 501, 0},
	} {
		var env envelope
		_ = json.Unmarshal(ExecutorErrorEnvelope(test.err), &env)
		if !reflect.DeepEqual([]any{env.Error.Code, env.Error.HTTPStatus, env.Error.RetryAfterSeconds}, []any{test.code, test.status, test.wait}) {
			t.Fatalf("error envelope %+v", env.Error)
		}
	}
}

func TestNativeBootstrapPreservesUnloadedPhysicalFile(t *testing.T) {
	isolatedState(t)
	nativeAuthEnabled.Store(true)
	t.Cleanup(func() { nativeAuthEnabled.Store(false); nativeAuthRoot.Store("") })
	dir := t.TempDir()
	nativeAuthRoot.Store(dir)
	state.Keys = []apiKey{{ID: "one", Enabled: true, Priority: 0, Secret: "private"}}
	name := nativeAuthFile("one")
	doc := map[string]any{"type": pluginID, "bridge_managed": true, "bridge_key_id": "one", "priority": 99, "weight": 8, "disabled": true, "disable_cooling": true}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	saved := false
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[]}`), nil
		}
		if method != "host.auth.save" {
			t.Fatalf("unexpected callback %s", method)
		}
		encoded, _ := json.Marshal(payload)
		var request struct {
			JSON map[string]any `json:"json"`
		}
		_ = json.Unmarshal(encoded, &request)
		if request.JSON["weight"] != float64(8) || request.JSON["priority"] != float64(99) || request.JSON["disabled"] != true || request.JSON["disable_cooling"] != true {
			t.Fatalf("bootstrap clobbered policy: %s", encoded)
		}
		saved = true
		return json.RawMessage(`{}`), nil
	}
	if err := syncNativeAuths(nil, true); err != nil || !saved {
		t.Fatalf("bootstrap %v, saved %v", err, saved)
	}
}
