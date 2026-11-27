package bridge

import (
	"encoding/json"
	"net/url"
	"strings"
)

type proxyEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type keyProxyView struct {
	Mode    string `json:"proxy_mode"`
	ID      string `json:"proxy_id,omitempty"`
	Label   string `json:"proxy_label"`
	Address string `json:"proxy_address,omitempty"`
}

func configuredKeyProxy(settings pluginState, key apiKey) (string, error) {
	switch key.ProxyMode {
	case "", "inherit":
		return "", nil
	case "direct":
		return "direct", nil
	case "pool":
		for _, entry := range settings.Proxies {
			if entry.ID == key.ProxyID {
				return entry.URL, nil
			}
		}
		return "", &statusError{400, "绑定的代理不存在，请重新选择"}
	default:
		return "", &statusError{400, "无效的凭证代理模式"}
	}
}

func normalizedKeyProxyMode(mode string) string {
	if mode == "" {
		return "inherit"
	}
	return mode
}

func sameProxy(a, b string) bool {
	left, e1 := normalizeProxyURL(a)
	right, e2 := normalizeProxyURL(b)
	return e1 == nil && e2 == nil && left == right
}

func nativeProxyDocument(key apiKey) (map[string]any, bool) {
	// Management HTTP calls have no selected auth context. Read the same physical
	// reference file so a CPA proxy edit applies even before its watcher reparses it.
	if nativeAuthEnabled.Load() {
		if doc, err := readNativeFile(nativeAuthFile(key.ID), ""); err == nil && authString(doc, "bridge_key_id") == key.ID {
			return doc, true
		}
	}
	if doc, ok := nativeMetadata.Load(key.ID); ok {
		return doc.(map[string]any), true
	}
	return nil, false
}

func credentialProxyURL(settings pluginState, key apiKey) (string, error) {
	var raw string
	if key.NativeProxyURL != nil {
		raw = *key.NativeProxyURL
	} else if doc, exists := nativeProxyDocument(key); exists {
		raw = authString(doc, "proxy_url")
	} else {
		var err error
		raw, err = configuredKeyProxy(settings, key)
		if err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(raw) == "" {
		raw = settings.ProxyURL
	}
	value, err := normalizeProxyURL(raw)
	if err != nil {
		return "", &statusError{400, "凭证代理地址无效，请检查 CPA 或插件中的代理设置"}
	}
	return value, nil
}

func maskedProxyURL(raw string) string {
	if raw == "direct" || raw == "none" {
		return "直连"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "无效代理"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

func publicKeyProxy(settings pluginState, key apiKey) keyProxyView {
	view := keyProxyView{Mode: normalizedKeyProxyMode(key.ProxyMode), ID: key.ProxyID}
	if doc, exists := nativeProxyDocument(key); exists {
		owned := authString(doc, "bridge_proxy_mode") != "" && sameProxy(authString(doc, "proxy_url"), authString(doc, "bridge_proxy_url"))
		if !owned {
			view.Mode, view.ID = "inherit", ""
			if strings.TrimSpace(authString(doc, "proxy_url")) != "" {
				view.Mode = "native"
			}
		}
	}
	resolved, err := credentialProxyURL(settings, key)
	if err != nil {
		view.Label = "代理配置无效"
		return view
	}
	view.Address = maskedProxyURL(resolved)
	switch view.Mode {
	case "pool":
		view.Label = "代理池"
		for _, entry := range settings.Proxies {
			if entry.ID == view.ID {
				view.Label = "代理池 · " + entry.Name
				break
			}
		}
	case "direct":
		view.Label = "独立直连"
	case "native":
		view.Label = "CPA 独立代理"
	default:
		if settings.ProxyURL != "" {
			view.Label = "插件默认代理"
		} else {
			view.Label = "CPA 全局配置"
			view.Address = ""
		}
	}
	return view
}

// Only explicit proxy edits or values still owned by the pool may overwrite
// CPA's proxy_url. Changing it in CPA relinquishes pool ownership.
func syncNativeKeyProxy(doc map[string]any, key apiKey, settings pluginState, old apiKey, hadOld, existing bool) (bool, error) {
	explicit := hadOld && (old.ProxyMode != key.ProxyMode || old.ProxyID != key.ProxyID)
	owned := authString(doc, "bridge_proxy_mode") != "" && sameProxy(authString(doc, "proxy_url"), authString(doc, "bridge_proxy_url"))
	if existing && !explicit && !owned {
		return false, nil
	}
	desired, err := configuredKeyProxy(settings, key)
	if err != nil {
		return false, err
	}
	mode := normalizedKeyProxyMode(key.ProxyMode)
	changed := !sameProxy(authString(doc, "proxy_url"), desired) || authString(doc, "bridge_proxy_mode") != mode || authString(doc, "bridge_proxy_id") != key.ProxyID
	if mode == "inherit" {
		changed = (explicit || owned) && (authString(doc, "proxy_url") != "" || authString(doc, "bridge_proxy_mode") != "")
		if explicit || owned {
			delete(doc, "proxy_url")
		}
		delete(doc, "bridge_proxy_mode")
		delete(doc, "bridge_proxy_id")
		delete(doc, "bridge_proxy_url")
	} else {
		doc["proxy_url"] = desired
		doc["bridge_proxy_mode"] = mode
		doc["bridge_proxy_id"] = key.ProxyID
		doc["bridge_proxy_url"] = desired
	}
	return changed, nil
}

func saveProxyEntry(body []byte, editing bool) ([]byte, error) {
	var input proxyEntry
	if json.Unmarshal(body, &input) != nil {
		return managementJSONStatus(400, map[string]string{"error": "代理参数无效"})
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 128 {
		return managementJSONStatus(400, map[string]string{"error": "请输入代理名称（最多 128 字节）"})
	}
	var err error
	input.URL, err = normalizeProxyURL(input.URL)
	if err != nil || input.URL == "" || input.URL == "direct" {
		return managementJSONStatus(400, map[string]string{"error": "请输入有效的 HTTP(S) 或 SOCKS5(H) 代理地址"})
	}
	err = mutateState(func(s *pluginState) error {
		for i, entry := range s.Proxies {
			if editing && entry.ID == input.ID {
				s.Proxies[i] = input
				return nil
			}
		}
		if editing {
			return &statusError{404, "代理不存在"}
		}
		input.ID = randomID()
		s.Proxies = append(s.Proxies, input)
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	closeProxyTransport()
	return managementJSON(input)
}

func deleteProxyEntry(body []byte) ([]byte, error) {
	var input struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &input) != nil || input.ID == "" {
		return managementJSONStatus(400, map[string]string{"error": "请选择代理"})
	}
	err := mutateState(func(s *pluginState) error {
		for _, key := range s.Keys {
			if key.ProxyMode == "pool" && key.ProxyID == input.ID {
				return &statusError{409, "代理仍被凭证绑定，请先修改凭证的代理设置"}
			}
		}
		for i, entry := range s.Proxies {
			if entry.ID == input.ID {
				s.Proxies = append(s.Proxies[:i], s.Proxies[i+1:]...)
				return nil
			}
		}
		return &statusError{404, "代理不存在"}
	})
	if err != nil {
		return mutationError(err)
	}
	closeProxyTransport()
	return managementJSON(map[string]bool{"ok": true})
}
