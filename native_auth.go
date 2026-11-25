package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// CPA owns selection, credential weights, retry policy and error cooldowns.
// Managed files reference the existing plugin key store without duplicating keys.
var nativeAuthEnabled atomic.Bool
var nativeAuthRoot atomic.Value // resolved CPA auth directory, independent of plugin data override
var nativeSyncMu sync.Mutex
var nativeEditMu sync.Mutex
var nativeMetadata sync.Map // bridge key ID -> immutable CPA metadata snapshot

type nativeAuthData struct {
	Provider    string
	ID          string
	FileName    string
	Label       string
	Prefix      string
	ProxyURL    string
	Disabled    bool
	StorageJSON []byte
	Metadata    map[string]any
	Attributes  map[string]string
}

type nativeAuthRequest struct {
	Provider     string
	FileName     string
	RawJSON      []byte
	AuthID       string
	AuthProvider string
	StorageJSON  []byte
	Metadata     map[string]any
	Attributes   map[string]string
	Host         struct{ AuthDir string }
}

func nativeAuthFile(keyID string) string {
	sum := sha256.Sum256([]byte(keyID))
	return pluginID + "-" + hex.EncodeToString(sum[:16]) + ".json"
}

func authDocument(raw []byte, metadata map[string]any) (map[string]any, error) {
	doc := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			return nil, errors.New("invalid Mistral auth JSON")
		}
	}
	for name, value := range metadata {
		doc[name] = value
	}
	return doc, nil
}

func authString(doc map[string]any, name string) string { value, _ := doc[name].(string); return value }
func authBool(doc map[string]any, name string) bool     { value, _ := doc[name].(bool); return value }

func parseNativeAuth(raw []byte) ([]byte, error) {
	var req nativeAuthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != pluginID {
		return okEnvelope(map[string]bool{"Handled": false})
	}
	configureDataDirectory(req.Host.AuthDir)
	doc, err := authDocument(req.RawJSON, nil)
	if err != nil {
		return nil, err
	}
	if authString(doc, "type") != pluginID {
		return okEnvelope(map[string]bool{"Handled": false})
	}
	disabled := authBool(doc, "disabled")
	label := authString(doc, "label")
	if id := authString(doc, "bridge_key_id"); id != "" {
		key, exists := getNativeKey(id)
		if !exists {
			disabled = true
		}
		if label == "" {
			label = key.Label
		}
		nativeMetadata.Store(id, doc)
	} else if strings.TrimSpace(authString(doc, "api_key")) == "" {
		return nil, errors.New("Mistral auth requires bridge_key_id or api_key")
	}
	attributes := map[string]string{"api_key": "plugin-managed"}
	// Do not set base_url/compat_name: those select CPA's native compat executor.
	for _, name := range []string{"priority", "weight"} {
		if value, exists := doc[name]; exists {
			attributes[name] = fmt.Sprint(value)
			if number, ok := value.(float64); ok {
				attributes[name] = strconv.FormatFloat(number, 'f', -1, 64)
			}
		}
	}
	if excluded, ok := doc["excluded_models"].([]any); ok {
		var names []string
		for _, value := range excluded {
			if name, ok := value.(string); ok {
				names = append(names, name)
			}
		}
		attributes["excluded_models"] = strings.Join(names, ",")
	}
	data := nativeAuthData{Provider: pluginID, FileName: req.FileName, Label: label, Prefix: authString(doc, "prefix"), ProxyURL: authString(doc, "proxy_url"), Disabled: disabled, StorageJSON: req.RawJSON, Metadata: doc, Attributes: attributes}
	return okEnvelope(map[string]any{"Handled": true, "Auth": data})
}

func getNativeKey(id string) (apiKey, bool) {
	for _, key := range currentState().Keys {
		if key.ID == id {
			return key, true
		}
	}
	return apiKey{}, false
}

func selectedNativeKey(req rpcExecutorRequest) (apiKey, error) {
	if req.AuthID == "" || req.AuthProvider != pluginID {
		return apiKey{}, &statusError{503, "CPA did not select a Mistral credential; synchronize native credentials first"}
	}
	doc, err := authDocument(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return apiKey{}, &statusError{401, err.Error()}
	}
	if authBool(doc, "disabled") {
		return apiKey{}, &statusError{401, "Selected Mistral credential is disabled"}
	}
	if id := authString(doc, "bridge_key_id"); id != "" {
		key, exists := getNativeKey(id)
		if !exists {
			return apiKey{}, &statusError{401, "Selected Mistral credential is no longer available"}
		}
		return key, nil
	}
	key := apiKey{ID: req.AuthID, Secret: strings.TrimSpace(authString(doc, "api_key")), Label: authString(doc, "label"), BaseURL: authString(doc, "mistral_base_url"), Enabled: true, Channel: authString(doc, "channel"), Source: authString(doc, "source")}
	if key.Secret == "" {
		return apiKey{}, &statusError{401, "Selected Mistral credential has no API key"}
	}
	return key, nil
}

func nativeModels(raw []byte) ([]byte, error) {
	var req nativeAuthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	configureDataDirectory(req.Host.AuthDir)
	models := make([]map[string]any, 0)
	if req.AuthProvider != pluginID {
		return okEnvelope(map[string]any{"Provider": pluginID, "Models": models})
	}
	key, err := selectedNativeKey(rpcExecutorRequest{AuthID: req.AuthID, AuthProvider: req.AuthProvider, StorageJSON: req.StorageJSON, AuthMetadata: req.Metadata})
	if err == nil {
		for _, model := range currentState().Models {
			if model.Enabled && (model.KeyID == "" || model.KeyID == key.ID) {
				models = append(models, modelInfo(model))
			}
		}
	}
	return okEnvelope(map[string]any{"Provider": pluginID, "Models": models})
}

func refreshNativeAuth(raw []byte) ([]byte, error) {
	var req nativeAuthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{"Auth": nativeAuthData{Provider: pluginID, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}})
}

// Called only outside state locks: host.auth.save may re-enter auth.parse and
// model.for_auth synchronously. Preserve all host-owned metadata on every save.
func syncNativeAuths(before *pluginState, force bool) error {
	if !nativeAuthEnabled.Load() {
		return nil
	}
	nativeSyncMu.Lock()
	defer nativeSyncMu.Unlock()
	snapshot := currentState()
	raw, err := hostCall("host.auth.list", map[string]any{})
	if err != nil {
		return fmt.Errorf("list CPA credentials: %w", err)
	}
	var listing struct {
		Files []struct {
			Name      string `json:"name"`
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return err
	}
	files := make(map[string]string)
	for _, file := range listing.Files {
		files[file.Name] = file.AuthIndex
	}
	// A live but not-yet-loaded auth manager lists no physical files. Merge only
	// our reference filenames so startup never overwrites persisted CPA options.
	if root, _ := nativeAuthRoot.Load().(string); root != "" {
		entries, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && strings.HasPrefix(name, pluginID+"-") && strings.HasSuffix(name, ".json") {
				if _, exists := files[name]; !exists {
					files[name] = ""
				}
			}
		}
	}
	previous := make(map[string]apiKey)
	if before != nil {
		for _, key := range before.Keys {
			previous[key.ID] = key
		}
	}
	current := make(map[string]bool)
	for _, key := range snapshot.Keys {
		name := nativeAuthFile(key.ID)
		current[name] = true
		doc := map[string]any{"type": pluginID, "bridge_managed": true, "bridge_key_id": key.ID, "label": key.Label, "priority": key.Priority, "disabled": !key.Enabled}
		index, existing := files[name]
		if existing {
			loaded, err := readNativeFile(name, index)
			if err != nil {
				return err
			}
			if authString(loaded, "bridge_key_id") != key.ID || !authBool(loaded, "bridge_managed") {
				return fmt.Errorf("CPA credential filename collision: %s", name)
			}
			doc = loaded
			if old, ok := previous[key.ID]; ok {
				if old.Label != key.Label {
					doc["label"] = key.Label
				}
				if old.Priority != key.Priority {
					doc["priority"] = key.Priority
				}
				if old.Enabled != key.Enabled {
					doc["disabled"] = !key.Enabled
				}
			}
		}
		savedRevision := authString(doc, "bridge_models_revision")
		// A revision change makes CPA refresh model eligibility, including bindings.
		doc["bridge_models_revision"] = modelRevision(snapshot.Models)
		encoded, _ := json.Marshal(doc)
		if existing && index != "" && before == nil && !force && savedRevision == modelRevision(snapshot.Models) {
			nativeMetadata.Store(key.ID, doc)
			continue
		}
		if _, err := hostCall("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(encoded)}); err != nil {
			return fmt.Errorf("save CPA credential %s: %w", name, err)
		}
		nativeMetadata.Store(key.ID, doc)
	}
	// Only remove files bearing our ownership marker and deterministic name.
	for name, index := range files {
		if current[name] || !strings.HasPrefix(name, pluginID+"-") {
			continue
		}
		doc, err := readNativeFile(name, index)
		if err != nil {
			return err
		}
		id := authString(doc, "bridge_key_id")
		if !authBool(doc, "bridge_managed") || name != nativeAuthFile(id) {
			continue
		}
		doc["disabled"] = true
		encoded, _ := json.Marshal(doc)
		saved, err := hostCall("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(encoded)})
		if err != nil {
			return err
		}
		var result struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(saved, &result); err != nil {
			return err
		}
		// The host has already disabled it; its file watcher handles removal.
		if result.Path != "" && filepath.Base(result.Path) == name {
			if err := os.Remove(result.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		nativeMetadata.Delete(id)
	}
	return nil
}

func readNativeAuth(index string) (map[string]any, error) {
	raw, err := hostCall("host.auth.get", map[string]string{"auth_index": index})
	if err != nil {
		return nil, err
	}
	var result struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return authDocument(result.JSON, nil)
}

func readNativeFile(name, index string) (map[string]any, error) {
	if index != "" {
		return readNativeAuth(index)
	}
	// Static model registration can run before CPA loads its persisted auths.
	// Preserve the physical document even when it has no runtime index yet.
	root, _ := nativeAuthRoot.Load().(string)
	if root == "" || filepath.Base(name) != name {
		return nil, errors.New("CPA auth directory unavailable")
	}
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return nil, err
	}
	return authDocument(raw, nil)
}

func nativeEnabled(key apiKey) bool {
	if value, ok := nativeMetadata.Load(key.ID); ok {
		if disabled, exists := value.(map[string]any)["disabled"].(bool); exists {
			return !disabled
		}
	}
	return key.Enabled
}

// Explicit console edits also work when CPA's value differs from the legacy
// plugin snapshot. Unrelated edits do not rewrite CPA's scheduling policy.
func nativeEditBaseline(before *pluginState, body []byte, toggle bool) {
	var input struct {
		ID       string `json:"id"`
		Enabled  *bool  `json:"enabled"`
		Priority *int   `json:"priority"`
	}
	if json.Unmarshal(body, &input) != nil {
		return
	}
	for i := range before.Keys {
		if before.Keys[i].ID != input.ID {
			continue
		}
		if input.Enabled != nil {
			before.Keys[i].Enabled = !*input.Enabled
		} else if toggle {
			before.Keys[i].Enabled = nativeEnabled(before.Keys[i])
		}
		if input.Priority != nil {
			before.Keys[i].Priority = *input.Priority + 1
		}
	}
}

func nativePriority(key apiKey) int {
	if value, ok := nativeMetadata.Load(key.ID); ok {
		if priority, err := strconv.Atoi(fmt.Sprint(value.(map[string]any)["priority"])); err == nil {
			return priority
		}
	}
	return key.Priority
}

func nativeManagementError(err error) ([]byte, error) {
	return managementJSONStatus(http.StatusServiceUnavailable, map[string]string{"error": "CPA 凭证同步失败，请重试保存或同步；已保存的插件配置保留：" + err.Error()})
}
