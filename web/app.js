(() => {
  const $ = id => document.getElementById(id);
  const apiRoot = "/v0/management/plugins/mistral-bridge";
  const sessionKeyName = "mistral-bridge-management-key";
  const panelStorageKey = "cli-proxy-auth";
  const storagePrefixes = ["enc::v1::", "enc::v2::"];
  const storageSalt = "cli-proxy-api-webui::secure-storage";
  let sessionKeySuppressed = false;
  let managementKey = readSessionKey() || readPanelKey();
  let snapshot = { keys: [], catalog: [], models: [], debug_mode: false, proxy_url: "" };
  let range = "all";
  let page = 1;
  let totalPages = 1;
  let logsController;
  let toastTimer;
  let searchTimer;
  let publishing = false;
  let debugExchange;
  let debugPane = "original_request";
  let requestEditBusy = false;
  let previewResultText = "";
  let previewResultMode = "tree";
  let previewInputRevision = 0;
  let previewRun = 0;
  let previewImportRun = 0;
	let previewSourceFormat = "";
  let previewDebugPage = 1;
  let previewDebugPages = 1;
  let previewDebugController;
  let previewDebugTimer;
  const jsonViews = new Map();
  const debugRequestNames = { original_request: "原始请求", executor_request: "执行器输入", upstream_request: "上游请求" };
  const previewInputs = {
    body: { input: "previewRequestBody", structured: "previewBodyStructured", tree: "previewBodyTree" },
    headers: { input: "previewRequestHeaders", structured: "previewHeadersStructured", tree: "previewHeadersTree" }
  };
  const ruleActions = { set: "设置值", copy: "复制字段", move: "移动字段", delete: "删除字段" };
  const selectedModels = new Set();
  const refreshingKeys = new Set();
  const switchingKeys = new Set();
      function readSessionKey() {
        if (sessionKeySuppressed) return "";
        try { return sessionStorage.getItem(sessionKeyName) || ""; }
        catch { return ""; }
      }

      function decodePanelStorage(value) {
        const prefix = storagePrefixes.find(candidate => value.startsWith(candidate));
        if (!prefix) return value;
        try {
          const binary = atob(value.slice(prefix.length));
          const bytes = Uint8Array.from(binary, character => character.charCodeAt(0));
          const keyMaterial = prefix === "enc::v1::"
            ? `${storageSalt}|${window.location.host}|${navigator.userAgent}`
            : `${storageSalt}|v2|${window.location.host}`;
          const salt = new TextEncoder().encode(keyMaterial);
          for (let index = 0; index < bytes.length; index += 1) bytes[index] ^= salt[index % salt.length];
          return new TextDecoder().decode(bytes);
        } catch { return ""; }
      }

      function readPanelKey() {
        try {
          if (window.self === window.top) return "";
          const stored = localStorage.getItem(panelStorageKey);
          if (!stored) return "";
          const parsed = JSON.parse(decodePanelStorage(stored));
          const state = parsed?.state || parsed || {};
          return typeof state.managementKey === "string" ? state.managementKey : "";
        } catch { return ""; }
      }

      function storeSessionKey(value) {
        try {
          sessionStorage.setItem(sessionKeyName, value);
          sessionKeySuppressed = false;
          return true;
        } catch { return false; }
      }

      function clearSessionKey() {
        sessionKeySuppressed = true;
        try { sessionStorage.removeItem(sessionKeyName); }
        catch {}
      }


  function escapeHTML(value) {
    return String(value ?? "").replace(/[&<>"']/g, character => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[character]);
  }
  const number = value => Number(value || 0).toLocaleString("zh-CN");
  const duration = value => value == null ? "—" : `${(Number(value) / 1000).toFixed(2)} s`;
  const dateText = value => value ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "—";
  function notify(message, error = false) {
    $("toast").textContent = message;
    $("toast").className = `toast${error ? " error-text" : ""}`;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => $("toast").classList.add("hidden"), error ? 6000 : 2800);
  }
  function showError(error) {
    if (error.name === "AbortError") return;
    $("pageError").textContent = error.message;
    $("pageError").classList.remove("hidden");
  }
  async function request(path, options = {}, raw = false) {
    const headers = new Headers(options.headers);
    headers.set("Authorization", `Bearer ${managementKey}`);
    if (options.body) headers.set("Content-Type", "application/json");
    const response = await fetch(path, { ...options, headers });
    const responseText = await response.text();
    let data;
    try { data = JSON.parse(responseText); } catch { data = {}; }
    if (response.status === 401 || response.status === 403) {
      clearSessionKey(); managementKey = ""; showAuth();
    }
    if (!response.ok) throw new Error(data.error || data.message || `请求失败（HTTP ${response.status}）`);
    return raw ? responseText : data;
  }
  const api = (path, options, raw) => request(`${apiRoot}${path}`, options, raw);
  const jsonOptions = (method, body) => ({ method, body: JSON.stringify(body) });
  async function busy(button, action) {
    if (button.disabled) return;
    button.disabled = true; button.setAttribute("aria-busy", "true");
    try { await action(); }
    catch (error) { if (error.name !== "AbortError") notify(error.message, true); }
    finally { button.disabled = false; button.removeAttribute("aria-busy"); }
  }
  function showAuth() {
    $("authGate").classList.remove("hidden"); $("app").classList.add("hidden");
  }
  async function connect() {
    try {
      await loadState();
      $("authGate").classList.add("hidden"); $("app").classList.remove("hidden");
    } catch (error) { $("authError").textContent = error.message; showAuth(); return; }
    loadLogs().catch(showError);
  }
  async function loadState() {
    snapshot = await api("/state");
    snapshot.keys ||= []; snapshot.models ||= []; snapshot.catalog ||= [];
    snapshot.debug_mode = Boolean(snapshot.debug_mode);
    snapshot.proxy_url ||= "";
    snapshot.request_edit ||= { builtin_enabled: true, auto_drop_incompatible: true, rules: [] };
    snapshot.request_edit.auto_drop_incompatible ??= true;
    snapshot.request_edit.rules ||= [];
    renderRequestEdit();
    renderProxy();
    $("debugModeToggle").setAttribute("aria-checked", String(snapshot.debug_mode));
    renderKeys(); renderModels();
    $("catalogOptions").innerHTML = snapshot.catalog.map(model => `<option value="${escapeHTML(model)}"></option>`).join("");
    const keyValue = $("mappingKey").value;
    $("mappingKey").innerHTML = '<option value="">由 CPA 自动调度</option>' + snapshot.keys.map(key => `<option value="${escapeHTML(key.id)}">${escapeHTML(key.label)} · ${escapeHTML(key.masked)}</option>`).join("");
    $("mappingKey").value = keyValue;
    $("publishNotice").classList.toggle("hidden", snapshot.models_revision === snapshot.published_revision);
  }
  function showView(name) {
    document.querySelectorAll("[data-page]").forEach(element => { element.hidden = element.dataset.page !== name; });
    document.querySelectorAll("[data-view]").forEach(element => {
      const selected = element.dataset.view === name;
      element.setAttribute("aria-selected", String(selected)); element.tabIndex = selected ? 0 : -1;
    });
    if (name === "requests") loadLogs().catch(showError);
    if (name === "keys") refreshQuotas(false);
  }
  function renderProxy() {
    // Background quota refreshes must not overwrite an unsaved proxy edit.
    if (!$("proxyForm").contains(document.activeElement) && $("proxyUrl").dataset.dirty !== "true") {
      $("proxyUrl").value = snapshot.proxy_url;
    }
    $("proxyStatus").textContent = snapshot.proxy_url ? "独立代理已配置" : "沿用 CPA 配置";
    $("proxyStatus").classList.toggle("success", Boolean(snapshot.proxy_url));
  }
  async function saveProxy(proxyUrl) {
    const controls = [...$("proxyForm").querySelectorAll("button, input")];
    controls.forEach(control => { control.disabled = true; });
    $("proxyForm").setAttribute("aria-busy", "true");
    $("proxyError").textContent = "";
    try {
      const result = await api("/proxy", jsonOptions("PATCH", { proxy_url: proxyUrl }));
      snapshot.proxy_url = result.proxy_url || "";
      $("proxyUrl").value = snapshot.proxy_url;
      delete $("proxyUrl").dataset.dirty;
      renderProxy();
      notify(snapshot.proxy_url ? "独立代理已保存，新请求将使用此代理" : "已恢复沿用 CPA 的代理配置");
    } catch (error) { $("proxyError").textContent = error.message; }
    finally {
      controls.forEach(control => { control.disabled = false; });
      $("proxyForm").removeAttribute("aria-busy");
    }
  }
  function renderRequestEdit() {
    const config = snapshot.request_edit;
    const toggle = $("builtinMappingsToggle");
    toggle.setAttribute("aria-checked", String(config.builtin_enabled));
    toggle.querySelector("span:last-child").textContent = config.builtin_enabled ? "已启用" : "已关闭";
    const dropToggle = $("autoDropToggle");
    dropToggle.setAttribute("aria-checked", String(config.auto_drop_incompatible));
    dropToggle.querySelector("span:last-child").textContent = config.auto_drop_incompatible ? "已启用" : "已关闭";
    $("requestRuleCount").textContent = `${config.rules.length} 条规则 · ${config.rules.filter(rule => rule.enabled).length} 条启用`;
    const fieldText = (scope, path) => `<div class="sub">${scope === "header" ? "标头" : "请求体"}</div><span class="mono">${escapeHTML(path)}</span>`;
    $("requestRuleRows").innerHTML = config.rules.length ? config.rules.map((rule, index) => {
      const source = rule.action === "set" ? `<span class="mono">${escapeHTML((rule.value_json || "").slice(0, 120))}</span>` : rule.action === "delete" ? "—" : fieldText(rule.source, rule.from);
      return `<tr><td><div class="rule-order"><span>${index + 1}</span><button class="btn small" type="button" data-rule-up="${escapeHTML(rule.id)}" aria-label="上移规则 ${escapeHTML(rule.name)}" ${index === 0 ? "disabled" : ""}>↑</button><button class="btn small" type="button" data-rule-down="${escapeHTML(rule.id)}" aria-label="下移规则 ${escapeHTML(rule.name)}" ${index === config.rules.length - 1 ? "disabled" : ""}>↓</button></div></td>
        <td class="rule-field"><strong>${escapeHTML(rule.name)}</strong><div class="sub">${ruleActions[rule.action]}${rule.action !== "delete" && !rule.overwrite ? " · 保留已有目标" : ""}</div></td><td class="rule-field">${source}</td><td class="rule-field">${fieldText(rule.target, rule.path)}</td>
        <td><button class="switch" type="button" role="switch" aria-checked="${rule.enabled}" data-rule-toggle="${escapeHTML(rule.id)}" aria-label="启用规则 ${escapeHTML(rule.name)}"><span class="switch-track"></span><span>${rule.enabled ? "已启用" : "未启用"}</span></button></td>
        <td><div class="actions"><button class="btn small" type="button" data-rule-edit="${escapeHTML(rule.id)}">编辑</button><button class="btn small btn-danger" type="button" data-rule-delete="${escapeHTML(rule.id)}">删除</button></div></td></tr>`;
    }).join("") : '<tr><td colspan="6" class="empty"><strong>还没有自定义规则</strong>添加规则，设置、复制、移动或删除请求字段</td></tr>';
    if (requestEditBusy) $("page-request-edit").querySelectorAll("button").forEach(button => { button.disabled = true; });
  }
  async function saveRequestEdit(change) {
    if (requestEditBusy) throw new Error("正在保存请求编辑配置，请稍后再试");
    requestEditBusy = true;
    const controls = [...document.querySelectorAll("#page-request-edit button, #requestRuleForm button, #requestRuleForm input, #requestRuleForm select, #requestRuleForm textarea")];
    controls.forEach(control => { control.disabled = true; });
    try {
      snapshot.request_edit = await api("/request-edit", jsonOptions("PATCH", change));
      invalidatePreviewResult();
      notify("请求编辑配置已保存，新请求立即生效");
    } finally {
      requestEditBusy = false;
      controls.forEach(control => { control.disabled = false; });
      renderRequestEdit(); updateRequestRuleFields();
    }
  }
  function updateRequestRuleFields() {
    const action = $("requestRuleAction").value;
    const source = action === "copy" || action === "move";
    $("requestRuleSourceFields").classList.toggle("hidden", !source);
    $("requestRuleFrom").required = source;
    $("requestRuleValueField").classList.toggle("hidden", action !== "set");
    $("requestRuleValue").required = action === "set";
    $("requestRuleOverwriteField").classList.toggle("hidden", action === "delete");
    $("requestRuleFromHint").textContent = $("requestRuleSource").value === "header" ? "输入标头名称，例如 X-Client-Tag；大小写不敏感。" : "JSON Pointer，例如 /metadata/project；支持 *。";
    $("requestRulePathHint").textContent = $("requestRuleTarget").value === "header" ? "上游标头名称，例如 X-Request-Tag；大小写不敏感。" : "JSON Pointer，例如 /temperature 或 /messages/*/content；复制或移动时，来源和目标的 * 数量须相同。";
    $("requestRuleValueHint").textContent = $("requestRuleTarget").value === "header" ? '标头值填写 JSON 字符串或字符串数组，例如 "example" 或 ["a", "b"]。' : '支持任意 JSON 值，例如 0.3、true、"example"、null 或 {"key":"value"}。';
  }
  function openRequestRule(rule = null) {
    $("requestRuleForm").reset();
    $("requestRuleId").value = rule?.id || "";
    $("requestRuleName").value = rule?.name || "";
    $("requestRuleAction").value = rule?.action || "set";
    $("requestRuleTarget").value = rule?.target || "body";
    $("requestRuleSource").value = rule?.source || "body";
    $("requestRuleFrom").value = rule?.from || "";
    $("requestRulePath").value = rule?.path || "";
    $("requestRuleValue").value = rule?.action === "set" ? rule.value_json : "";
    $("requestRuleOverwrite").checked = rule ? rule.overwrite : true;
    $("requestRuleEnabled").checked = rule ? rule.enabled : true;
    $("requestRuleDialogTitle").textContent = rule ? "编辑请求规则" : "添加请求规则";
    $("requestRuleError").textContent = "";
    updateRequestRuleFields(); $("requestRuleDialog").showModal();
  }
  async function changeRequestRule(id, action) {
    const rules = [...snapshot.request_edit.rules];
    const index = rules.findIndex(rule => rule.id === id);
    if (index < 0) return;
    if (action === "toggle") rules[index] = { ...rules[index], enabled: !rules[index].enabled };
    if (action === "delete") rules.splice(index, 1);
    if (action === "up" && index > 0) [rules[index - 1], rules[index]] = [rules[index], rules[index - 1]];
    if (action === "down" && index < rules.length - 1) [rules[index + 1], rules[index]] = [rules[index], rules[index + 1]];
    await saveRequestEdit({ rules });
  }
  function invalidatePreviewResult() {
    previewRun++;
    previewResultText = "";
    jsonViews.delete("result");
    $("requestPreviewResult").classList.add("hidden");
    $("requestPreviewOutput").replaceChildren();
    $("requestPreviewText").textContent = "";
  }
  function previewInputChanged() {
    previewInputRevision++;
    invalidatePreviewResult();
    $("requestPreviewError").textContent = "";
    if ($("previewDebugSource").dataset.recordId) {
      $("previewDebugSource").textContent = "已修改导入的请求，预览使用当前输入。";
      delete $("previewDebugSource").dataset.recordId;
    }
  }
  function setPreviewInputMode(name, mode) {
    const fields = previewInputs[name];
    if (mode === "tree") jsonViews.set(name, BridgeJSON.mount($(fields.tree), $(fields.input).value));
    $(fields.input).classList.toggle("hidden", mode === "tree");
    $(fields.structured).classList.toggle("hidden", mode !== "tree");
    document.querySelectorAll(`[data-json-input="${name}"]`).forEach(button => button.setAttribute("aria-pressed", String(button.dataset.jsonMode === mode)));
  }
  function setPreviewResultMode(mode) {
    previewResultMode = mode;
    $("requestPreviewOutput").classList.toggle("hidden", mode !== "tree");
    $("requestPreviewText").classList.toggle("hidden", mode !== "text");
    document.querySelectorAll("[data-json-output-mode]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.jsonOutputMode === mode)));
    $("requestPreviewResult").querySelectorAll("[data-json-expand], [data-json-collapse]").forEach(button => { button.disabled = mode !== "tree"; });
  }
  async function copyTextToClipboard(text) {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const input = document.createElement("textarea");
      input.value = text; input.style.position = "fixed"; input.style.opacity = "0";
      document.body.append(input); input.select(); document.execCommand("copy"); input.remove();
    }
  }
  async function loadPreviewDebugRecords() {
    previewDebugController?.abort();
    const controller = new AbortController(); previewDebugController = controller;
    const query = new URLSearchParams({ page: String(previewDebugPage), page_size: "10", search: $("previewDebugSearch").value.trim() });
    $("previewDebugError").textContent = "";
    $("previewDebugRows").setAttribute("aria-busy", "true");
    try {
      const data = await api(`/request-edit/debug-records?${query}`, { signal: controller.signal });
      if (controller.signal.aborted) return;
      previewDebugPage = data.page; previewDebugPages = data.pages;
      $("previewDebugCount").textContent = `${number(data.total)} 条调试记录`;
      $("previewDebugPage").textContent = `第 ${data.page} 页，共 ${data.pages} 页`;
      $("previewDebugPrev").disabled = data.page <= 1; $("previewDebugNext").disabled = data.page >= data.pages;
      const kind = $("previewDebugKind").value;
      $("previewDebugRows").innerHTML = data.records.length ? data.records.map(record => {
        const available = kind === "original_request" ? record.original_available : kind === "executor_request" ? record.executor_available : true;
        return `<tr>
        <td>${escapeHTML(dateText(record.created_at))}<div class="sub mono">${escapeHTML(record.id)}</div></td><td class="rule-field">${escapeHTML(record.model)}<div class="sub mono">${escapeHTML(record.upstream_model || "—")}</div></td>
        <td>${escapeHTML(record.key_label || "—")}<div class="sub">${record.stream ? "流式" : "普通"}</div></td><td><span class="badge ${record.status === "error" ? "error" : "success"}">${record.status === "error" ? "失败" : "成功"}${record.http_status ? ` · ${Number(record.http_status)}` : ""}</span><div class="sub">${record.original_available ? "已保存原始请求" : "未保存原始请求"}</div></td>
        <td><button class="btn small" type="button" data-preview-import="${escapeHTML(record.id)}" data-preview-kind="${kind}" aria-label="导入调试请求 ${escapeHTML(record.id)}" ${available ? "" : 'disabled title="此记录未保存所选来源，请切换导入内容"'}>${available ? "导入" : "来源不可用"}</button></td></tr>`;
      }).join("") : '<tr><td colspan="5" class="empty"><strong>没有可导入的调试记录</strong>调整搜索条件，或开启调试模式后发送请求，再刷新记录。</td></tr>';
    } catch (error) {
      if (error.name !== "AbortError") $("previewDebugError").textContent = error.message;
    } finally {
      if (previewDebugController === controller) $("previewDebugRows").removeAttribute("aria-busy");
    }
  }
  async function importPreviewDebugRecord(id, kind = "original_request") {
    const run = ++previewImportRun;
    const revision = previewInputRevision;
    const data = await api(`/request-edit/debug-record?id=${encodeURIComponent(id)}&kind=${encodeURIComponent(kind)}`);
    if (run !== previewImportRun) return;
    if (revision !== previewInputRevision) throw new Error("加载期间输入已修改，请重新导入以使用此记录");
    // Validate/format both values before changing either input. An expired,
    // malformed or oversized record must not erase a user's current work.
    const body = BridgeJSON.format(data.body_json);
    const headers = BridgeJSON.format(JSON.stringify(data.headers || {}));
    const source = data.source;
    $("previewRequestBody").value = body; $("previewRequestHeaders").value = headers;
    previewInputChanged();
    previewSourceFormat = data.format || "";
    for (const name of Object.keys(previewInputs)) setPreviewInputMode(name, "edit");
    $("previewDebugSource").textContent = `已导入 ${dateText(source.created_at)} · ${source.model || "调试请求"} · ${source.id}（脱敏${debugRequestNames[data.kind]}${data.format ? ` · ${data.format}` : ""}）`;
    $("previewDebugSource").dataset.recordId = source.id;
    $("importExecutorForPreview").dataset.recordId = source.id;
    $("importExecutorForPreview").classList.toggle("hidden", data.kind !== "original_request" || !data.format || data.format === "openai" || !source.executor_available);
    $("previewDebugDialog").close();
    $("debugDialog").close();
    $("requestPreviewPanel").open = true;
    showView("request-edit");
    notify(`${debugRequestNames[data.kind]}已导入预览，可继续查看或编辑`);
  }
  function logQuery() {
    const query = new URLSearchParams({ page: String(page), page_size: $("pageSize").value });
    for (const [id, name] of [["logSearch", "search"], ["logStatus", "status"], ["logModel", "model"], ["logKey", "key_id"], ["logThinking", "thinking"]]) {
      if ($(id).value.trim()) query.set(name, $(id).value.trim());
    }
    const now = new Date();
    if (range === "today") { const start = new Date(now); start.setHours(0, 0, 0, 0); query.set("from", start.toISOString()); }
    if (["24h", "7d", "30d"].includes(range)) {
      const days = { "24h": 1, "7d": 7, "30d": 30 }[range];
      query.set("from", new Date(now.getTime() - days * 86400000).toISOString());
    }
    if (range === "custom") {
      if (!$("rangeFrom").value || !$("rangeTo").value) throw new Error("请选择完整的起止时间");
      const from = new Date($("rangeFrom").value), to = new Date($("rangeTo").value);
      if (from > to) throw new Error("结束时间不能早于起始时间");
      query.set("from", from.toISOString()); query.set("to", new Date(to.getTime() + 60000).toISOString());
    }
    return query;
  }
  function setOptions(id, items, placeholder) {
    const value = $(id).value;
    $(id).innerHTML = `<option value="">${placeholder}</option>` + items.map(([key, label]) => `<option value="${escapeHTML(key)}">${escapeHTML(label)}</option>`).join("");
    if (items.some(([key]) => key === value)) $(id).value = value;
  }
  function droppedFieldsHTML(log) {
    if (!log.dropped_count) return "";
    const fields = Array.isArray(log.dropped_fields) ? log.dropped_fields : [];
    const items = fields.map(field => `<div class="dropped-field"><code>${escapeHTML(field.path)}</code><div class="sub">${escapeHTML(field.reason || "Mistral schema 不支持此字段")}${field.truncated ? " · 内容已截断" : ""}</div><pre>${escapeHTML(field.value)}</pre></div>`).join("");
    const remaining = log.dropped_count > fields.length ? `<div class="sub">另有 ${number(log.dropped_count - fields.length)} 项未展示，明细已达到保留上限。</div>` : "";
    return `<details class="log-dropped"><summary>自动丢弃 ${number(log.dropped_count)} 项</summary>${items}${remaining}</details>`;
  }
  async function loadLogs() {
    const query = logQuery();
    logsController?.abort();
    const controller = new AbortController(); logsController = controller;
    const data = await api(`/logs?${query}`, { signal: controller.signal });
    if (controller.signal.aborted) return;
    $("pageError").classList.add("hidden");
    const stats = data.stats;
    $("statRequests").textContent = number(stats.requests);
    $("statTokens").textContent = number(stats.total_tokens);
    $("statUsage").textContent = `输入 ${number(stats.input_tokens)} · 输出 ${number(stats.output_tokens)} · 缓存 ${stats.cache_samples ? number(stats.cached_tokens) : "—"}`;
    $("statCache").textContent = stats.cache_rate == null ? "—" : `${stats.cache_rate.toFixed(1)}%`;
    $("statSpeed").textContent = stats.token_rate == null ? "—" : `${stats.token_rate.toFixed(1)} t/s`;
    $("cacheSamples").textContent = stats.requests ? `${stats.cache_samples} 条请求返回缓存用量` : "";
    $("speedSamples").textContent = stats.requests ? `${stats.speed_samples} 条流式请求` : "";
    $("logCount").textContent = `${number(data.total)} 条`;
    $("logRetention").textContent = `已保留 ${number(data.retained)} 条 · 最多 ${number(data.retention_limit)} 条`;
    page = data.page; totalPages = data.pages;
    $("pageInfo").textContent = `第 ${page} 页，共 ${totalPages} 页`;
    $("prevPage").disabled = page <= 1; $("nextPage").disabled = page >= totalPages;
    const keys = new Map(Object.entries(data.keys || {}));
    snapshot.keys.forEach(key => keys.set(key.id, key.label));
    setOptions("logKey", [...keys], "全部凭证");
    setOptions("logModel", Object.keys(data.models || {}).sort().map(model => [model, model]), "全部模型");
    $("logRows").innerHTML = data.logs.length ? data.logs.map(log => `<tr>
      <td class="nowrap">${escapeHTML(new Date(log.created_at).toLocaleDateString("zh-CN"))}<div class="sub">${escapeHTML(new Date(log.created_at).toLocaleTimeString("zh-CN", {hour12:false}))}${log.stream ? " · 流式" : ""}</div></td>
      <td class="mono model-cell">${escapeHTML(log.model)}</td><td class="mono model-cell">${escapeHTML(log.upstream_model || "—")}</td>
      <td><span class="badge">${escapeHTML(log.thinking || "默认")}</span></td>
      <td><span class="badge ${log.status === "error" ? "error" : "success"}">${log.status === "error" ? "失败" : "成功"}${log.http_status ? ` · ${Number(log.http_status)}` : ""}</span>${log.error ? `<details class="log-error"><summary>错误详情</summary>${escapeHTML(log.error)}</details>` : ""}${droppedFieldsHTML(log)}</td>
      <td>${escapeHTML(log.key_label || "—")}<div class="sub mono">${escapeHTML(log.key_masked || "")}</div></td>
      <td class="nowrap">${duration(log.first_token_ms)}</td><td class="nowrap">${duration(log.duration_ms)}</td>
      <td class="nowrap">${log.usage_known || log.input_tokens || log.output_tokens ? `${number(log.total_tokens || (log.input_tokens || 0) + (log.output_tokens || 0))}<div class="sub">输入 ${number(log.input_tokens)} · 输出 ${number(log.output_tokens)}</div><div class="sub">缓存 ${log.cached_tokens == null ? "—" : number(log.cached_tokens)}</div>` : "—"}</td>
      <td>${log.debug_available ? `<button class="btn small" type="button" data-debug-log="${escapeHTML(log.id)}">查看</button>` : "—"}</td>
    </tr>`).join("") : '<tr><td colspan="10" class="empty"><strong>没有匹配的请求</strong>调整筛选条件或稍后刷新</td></tr>';
  }
  function formatDebugBody(body) {
    if (!body) return "（空响应体）";
    try { return BridgeJSON.format(body); }
    catch { return body; }
  }
  function debugText() {
    if (!debugExchange) return "";
    if (debugPane !== "response") {
      const request = debugExchange[debugPane];
      if (!request) return "此记录未保存此请求来源。旧版记录不能恢复原始请求；开启调试模式后发送新请求即可记录。";
      const headers = Object.entries(request.headers || {}).map(([name, values]) => `${name}: ${values.join(", ")}`).join("\n");
      const label = debugPane === "request" ? "实际上游请求" : debugPane === "original_request" ? "原始请求（CPA 协议转换与插件编辑前）" : "执行器输入（CPA 转换后，插件编辑前）";
      return [label + (request.format ? ` · ${request.format}` : ""), `${request.method || ""} ${request.url || ""}`.trim(), headers, formatDebugBody(request.body)].filter(Boolean).join("\n\n");
    }
    const response = debugExchange.response || {};
    const headers = Object.entries(response.headers || {}).map(([name, values]) => `${name}: ${values.join(", ")}`).join("\n");
    const state = response.complete ? "响应完整" : "响应不完整";
    const lines = [`HTTP ${response.status_code || "—"} · ${state}`, headers, formatDebugBody(response.body)];
    if (response.error) lines.splice(2, 0, `读取错误：${response.error}`);
    return lines.filter(Boolean).join("\n\n");
  }
  function renderDebugPane() {
    document.querySelectorAll("[data-debug-pane]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.debugPane === debugPane)));
    $("debugOutput").textContent = debugText();
    const field = debugPane === "response" ? "original_request" : debugPane;
    $("importOpenDebug").disabled = !debugExchange?.[field]?.body;
    $("importOpenDebug").textContent = field === "original_request" ? "导入原始请求预览" : field === "executor_request" ? "导入执行器输入预览" : "导入上游请求预览";
  }
  async function openDebugLog(id) {
    debugExchange = await api(`/logs/debug?id=${encodeURIComponent(id)}`);
    debugPane = debugExchange.original_request ? "original_request" : "request";
    $("debugSummary").textContent = `${dateText(debugExchange.created_at)} · ${duration(debugExchange.duration_ms)} · ${debugExchange.id}`;
    renderDebugPane();
    $("debugDialog").showModal();
  }
  async function copyDebugText() {
    const text = debugText();
    await copyTextToClipboard(text);
    notify("调试内容已复制");
  }
  function activeKeyChannel(key) {
    if (key.channel === "vibe" || key.channel === "studio") return key.channel;
    const source = key.source || key.quota?.kind;
    return source === "chat" ? "vibe" : source === "api" ? "studio" : "";
  }
  function renderKeyQuota(key, channel) {
    const kind = channel === "vibe" ? "chat" : "api";
    const quota = key.quotas?.[channel] || (key.quota?.kind === kind ? key.quota : null);
    const available = quota?.available;
    const active = activeKeyChannel(key) === channel;
    const remaining = available ? `${quota.remaining.toFixed(2)} ${escapeHTML(quota.currency || "")}` : "—";
    return `<div class="quota-pool ${active ? "active" : ""}" role="button" tabindex="0" aria-label="${escapeHTML(key.label)}：切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 渠道" aria-pressed="${active}" aria-disabled="${switchingKeys.has(key.id)}" data-channel-key="${escapeHTML(key.id)}" data-channel="${channel}" title="点击切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 渠道"><div class="quota-title"><span>${channel === "vibe" ? "Vibe" : "Studio / API"} 月度额度</span>${active ? '<span class="badge success">当前渠道</span>' : '<span class="muted">剩余</span>'}</div><div class="quota-value">${remaining}</div><div class="quota-track"><div class="quota-fill ${quota?.used_percent >= 90 ? "low" : ""}" style="width:${available ? Math.max(0, Math.min(100, 100 - quota.used_percent)) : 0}%"></div></div><div class="sub">${available ? `已用 ${quota.used.toFixed(2)} / ${quota.limit.toFixed(2)} ${escapeHTML(quota.currency || "")} · ${quota.used_percent.toFixed(1)}%` : escapeHTML(quota?.error || "尚未获取额度")}</div>${available && quota.error ? `<div class="sub error-text">${escapeHTML(quota.error)}，显示上次结果</div>` : ""}<div class="sub">${escapeHTML(quota?.plan || "套餐待查询")} · 重置 ${escapeHTML(dateText(quota?.reset_at))}</div>${available ? `<div class="sub">${quota.pay_as_you_go ? "已开启超额按量付费" : "按套餐额度使用"}</div>` : ""}<div class="sub">${quota?.updated_at ? `更新于 ${escapeHTML(dateText(quota.updated_at))}` : "额度来自 Mistral"}</div></div>`;
  }
  async function switchKeyChannel(id, channel) {
    if (switchingKeys.has(id) || snapshot.keys.find(key => key.id === id)?.channel === channel) return;
    const focused = document.activeElement;
    const restoreFocus = focused?.classList.contains("quota-pool") && focused.dataset.channelKey === id && focused.dataset.channel === channel;
    switchingKeys.add(id); renderKeys();
    try {
      const updated = await api("/keys/channel", jsonOptions("PATCH", { id, channel }));
      const index = snapshot.keys.findIndex(key => key.id === id);
      if (index >= 0) snapshot.keys[index] = updated;
      notify(channel === "auto" ? "已恢复按 Key 来源选择渠道，新请求生效" : `已切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 请求渠道，新请求生效`);
    } catch (error) { notify(error.message, true); }
    finally {
      switchingKeys.delete(id); renderKeys();
      if (restoreFocus) [...$("keyList").querySelectorAll(".quota-pool")].find(card => card.dataset.channelKey === id && card.dataset.channel === channel)?.focus();
    }
  }
  function renderKeys() {
    const query = $("keySearch").value.trim().toLowerCase();
    const keys = [...snapshot.keys].sort((a, b) => (b.priority || 0) - (a.priority || 0)).filter(key => `${key.label} ${key.masked} ${key.base_url}`.toLowerCase().includes(query));
    $("keyCount").textContent = `${keys.length} 个凭证`;
    $("keyList").innerHTML = keys.length ? keys.map(key => {
      const source = key.source || key.quota?.kind;
      const sourceName = source === "chat" ? "Vibe Key" : source === "api" ? "Studio Key" : source ? `其他来源（${source}）` : "待查询";
      const active = activeKeyChannel(key);
      const channelName = active === "vibe" ? "Vibe" : active === "studio" ? "Studio / API" : "待识别，可手动选择";
      return `<article class="key-card"><div class="key-main"><div><div class="key-name">${escapeHTML(key.label)}<span class="badge ${key.enabled ? "success" : ""}">${key.enabled ? "已启用" : "已停用"}</span></div><div class="sub mono">${escapeHTML(key.masked)}</div><div class="sub mono">${escapeHTML(key.base_url || "https://api.mistral.ai/v1")}</div><div class="key-meta"><span class="badge">优先级 ${Number(key.priority || 0)}</span></div></div>
      ${renderKeyQuota(key, "vibe")}${renderKeyQuota(key, "studio")}</div>
      <div class="key-channel"><div><div class="sub">创建来源：${escapeHTML(sourceName)}</div><div>当前请求渠道：<strong>${escapeHTML(channelName)}</strong>${key.enabled ? "" : "（凭证已停用）"}</div>${active === "studio" && source === "chat" ? '<div class="sub channel-note">Studio 渠道使用上游默认计费；Vibe 来源 Key 仍可能扣 Vibe 池。</div>' : ""}</div><div class="segments" role="group" aria-label="${escapeHTML(key.label)} 的使用渠道">${[["auto", "按来源"], ["vibe", "Vibe"], ["studio", "Studio / API"]].map(([channel, label]) => `<button type="button" data-channel-key="${escapeHTML(key.id)}" data-channel="${channel}" aria-pressed="${(key.channel || "auto") === channel}" ${switchingKeys.has(key.id) ? "disabled" : ""}>${label}</button>`).join("")}</div></div>
      <div class="key-bottom"><span class="sub">切换渠道后，新请求按所选渠道发送。</span><div class="actions"><button class="switch" role="switch" aria-checked="${key.enabled}" data-toggle-key="${escapeHTML(key.id)}" type="button"><span class="switch-track"></span>${key.enabled ? "已启用" : "已停用"}</button><button class="btn small" data-quota-key="${escapeHTML(key.id)}" type="button" ${refreshingKeys.has(key.id) ? "disabled" : ""}>${refreshingKeys.has(key.id) ? "刷新中…" : "↻ 额度"}</button><button class="btn small" data-test-key="${escapeHTML(key.id)}" type="button">测试</button><button class="btn small" data-edit-key="${escapeHTML(key.id)}" type="button">编辑</button><button class="btn small btn-danger" data-delete-key="${escapeHTML(key.id)}" type="button">删除</button></div></div></article>`;
    }).join("") : '<div class="panel empty"><strong>暂无匹配凭证</strong>添加 Mistral API Key 后即可配置模型。</div>';
  }
  async function refreshKeyQuota(id, force) {
    if (!id || refreshingKeys.has(id)) return;
    refreshingKeys.add(id); renderKeys();
    try {
      const updated = await api("/keys/quota", jsonOptions("POST", { id, force }));
      const index = snapshot.keys.findIndex(key => key.id === id);
      if (index >= 0) {
        // A slow quota response must not undo a channel switch or credential edit.
        const current = snapshot.keys[index];
        snapshot.keys[index] = { ...current, source: updated.source, quotas: updated.quotas };
      }
    } catch (error) { notify(error.message, true); }
    finally { refreshingKeys.delete(id); renderKeys(); }
  }
  async function refreshQuotas(force) {
    const pending = snapshot.keys.filter(key => force || !key.quotas || Date.now() - new Date(key.quotas.updated_at).getTime() >= 300000).map(key => key.id);
    const worker = async () => { while (pending.length) await refreshKeyQuota(pending.shift(), force); };
    await Promise.all([worker(), worker()]);
  }
  function openKey(key = null) {
    $("keyForm").reset(); $("keyId").value = key?.id || "";
    $("keyLabel").value = key?.label || "";
    $("baseUrl").value = key?.base_url || "https://api.mistral.ai/v1";
    $("keyPriority").value = key?.priority || 0;
    $("keyChannel").value = key?.channel || "auto";
    $("apiSecret").required = !key;
    $("apiSecret").placeholder = key ? "留空保留当前密钥" : "粘贴 API Key";
    $("keyDialogTitle").textContent = key ? "编辑凭证" : "添加凭证";
    $("secretHint").textContent = key ? "更换密钥后会重新获取额度。" : "保存后仅显示密钥尾号。";
    $("keyError").textContent = ""; $("keyDialog").showModal();
  }
  function visibleModels() {
    const query = $("modelSearch").value.trim().toLowerCase(), status = $("modelStatus").value;
    return snapshot.models.filter(model => `${model.client_model} ${model.upstream_model}`.toLowerCase().includes(query) && (!status || model.enabled === (status === "enabled")));
  }
  function renderModels() {
    const rows = visibleModels(), existing = new Set(snapshot.models.map(model => model.id));
    for (const id of selectedModels) if (!existing.has(id)) selectedModels.delete(id);
    $("modelCount").textContent = `${snapshot.models.filter(model => model.enabled).length} 个已启用 · ${rows.length} 个映射`;
    $("modelRows").innerHTML = rows.length ? rows.map(model => {
      const key = snapshot.keys.find(item => item.id === model.key_id);
      return `<tr><td><input type="checkbox" data-select-model="${escapeHTML(model.id)}" aria-label="选择 ${escapeHTML(model.client_model)}" ${selectedModels.has(model.id) ? "checked" : ""}></td><td class="mono model-cell">${escapeHTML(model.client_model)}</td><td class="mono model-cell">${escapeHTML(model.upstream_model)}</td><td class="mono">${escapeHTML(model.thinking_map || "自动传递")}</td><td>${escapeHTML(key?.label || "自动选择")}</td><td><button class="switch" role="switch" aria-label="启用 ${escapeHTML(model.client_model)}" aria-checked="${model.enabled}" data-toggle-model="${escapeHTML(model.id)}" type="button"><span class="switch-track"></span>${model.enabled ? "已启用" : "未启用"}</button></td><td><div class="actions"><button class="btn small" data-edit-model="${escapeHTML(model.id)}" type="button">编辑</button><button class="btn small btn-danger" data-delete-model="${escapeHTML(model.id)}" type="button">删除</button></div></td></tr>`;
    }).join("") : '<tr><td colspan="7" class="empty"><strong>暂无匹配映射</strong>拉取上游目录后，添加需要使用的模型。</td></tr>';
    $("catalogHint").textContent = `目录中有 ${snapshot.catalog.length} 个模型。拉取目录后，添加需要使用的映射，客户端使用“请求模型”名称。`;
    updateSelection();
  }
  function updateSelection() {
    const rows = visibleModels(), selected = rows.filter(model => selectedModels.has(model.id)).length;
    $("selectModels").checked = rows.length > 0 && selected === rows.length;
    $("selectModels").indeterminate = selected > 0 && selected < rows.length;
    $("selectionCount").textContent = `已选 ${selectedModels.size} 项`;
    $("enableSelected").disabled = selectedModels.size === 0;
    $("disableSelected").disabled = selectedModels.size === 0;
  }
  async function publishModels() {
    if (publishing) return;
    publishing = true;
    $("publishNotice").classList.remove("hidden"); $("publishText").textContent = "正在同步模型到 CPA…"; $("retryPublish").disabled = true;
    try {
      let requestedRevision = snapshot.models_revision;
      await request("/v0/management/plugins/mistral-bridge/config", jsonOptions("PATCH", { models_revision: requestedRevision }));
      for (let attempt = 0; attempt < 15; attempt++) {
        await new Promise(resolve => setTimeout(resolve, 400));
        await loadState();
        if (snapshot.models_revision === snapshot.published_revision) { notify("模型列表已同步到 CPA"); return; }
        if (snapshot.models_revision !== requestedRevision) {
          requestedRevision = snapshot.models_revision;
          await request("/v0/management/plugins/mistral-bridge/config", jsonOptions("PATCH", { models_revision: requestedRevision }));
        }
      }
      throw new Error("映射已保存，CPA 模型列表尚未完成同步，请点击重新同步");
    } catch (error) { $("publishText").textContent = error.message; $("publishNotice").classList.remove("hidden"); }
    finally { publishing = false; $("retryPublish").disabled = false; }
  }
  async function modelMutation(path, options) {
    await api(path, options); await loadState(); await publishModels();
  }
  function openMapping(model = null) {
    $("modelForm").reset(); $("mappingId").value = model?.id || "";
    $("upstreamModel").value = model?.upstream_model || ""; $("clientModel").value = model?.client_model || "";
    $("thinkingMap").value = model?.thinking_map || ""; $("mappingKey").value = model?.key_id || "";
    $("mappingEnabled").checked = model ? model.enabled : true;
    updateMappingEnabledLabel();
    $("modelDialogTitle").textContent = model ? "编辑模型映射" : "添加模型映射";
    $("modelError").textContent = ""; $("modelDialog").showModal();
  }
  function updateMappingEnabledLabel() {
    $("mappingEnabledLabel").textContent = $("mappingEnabled").checked ? "已启用" : "未启用";
  }
  $("mappingEnabled").addEventListener("change", updateMappingEnabledLabel);
  $("proxyUrl").addEventListener("input", () => { $("proxyUrl").dataset.dirty = "true"; $("proxyError").textContent = ""; });
  $("proxyForm").addEventListener("submit", event => {
    event.preventDefault();
    if ($("proxyForm").getAttribute("aria-busy") !== "true") saveProxy($("proxyUrl").value.trim());
  });
  $("resetProxy").addEventListener("click", () => saveProxy(""));
  $("builtinMappingsToggle").addEventListener("click", event => busy(event.currentTarget, () => saveRequestEdit({ builtin_enabled: !snapshot.request_edit.builtin_enabled })));
  $("autoDropToggle").addEventListener("click", event => busy(event.currentTarget, () => saveRequestEdit({ auto_drop_incompatible: !snapshot.request_edit.auto_drop_incompatible })));
  $("addRequestRule").addEventListener("click", () => openRequestRule());
  ["requestRuleAction", "requestRuleSource", "requestRuleTarget"].forEach(id => $(id).addEventListener("change", updateRequestRuleFields));
  $("requestRuleForm").addEventListener("submit", async event => {
    event.preventDefault();
    if (requestEditBusy) return;
    $("requestRuleError").textContent = "";
    const id = $("requestRuleId").value;
    const rule = { id, name: $("requestRuleName").value.trim(), enabled: $("requestRuleEnabled").checked, action: $("requestRuleAction").value, target: $("requestRuleTarget").value, path: $("requestRulePath").value.trim(), overwrite: $("requestRuleOverwrite").checked };
    try {
      if (rule.action === "set") {
        rule.value_json = $("requestRuleValue").value.trim();
        try { JSON.parse(rule.value_json); }
        catch { throw new Error("设置值不是合法 JSON；字符串请使用双引号"); }
      }
      if (rule.action === "copy" || rule.action === "move") { rule.source = $("requestRuleSource").value; rule.from = $("requestRuleFrom").value.trim(); }
      const rules = [...snapshot.request_edit.rules];
      const index = rules.findIndex(item => item.id === id);
      if (id && index < 0) throw new Error("此规则已被删除，请重新添加");
      if (index < 0) rules.push(rule); else rules[index] = rule;
      await saveRequestEdit({ rules }); $("requestRuleDialog").close();
    } catch (error) { $("requestRuleError").textContent = error.message; }
  });
  $("requestPreviewForm").addEventListener("submit", event => {
    event.preventDefault();
    busy(event.currentTarget.querySelector('[type="submit"]'), async () => {
      $("requestPreviewError").textContent = ""; invalidatePreviewResult();
      const run = previewRun;
      try {
        const body = BridgeJSON.compact($("previewRequestBody").value), headers = BridgeJSON.compact($("previewRequestHeaders").value);
        const result = await api("/request-edit/preview", { method: "POST", body: `{"body":${body},"headers":${headers},"source_format":${JSON.stringify(previewSourceFormat)}}` }, true);
        if (run !== previewRun) return;
        previewResultText = BridgeJSON.format(result);
        jsonViews.set("result", BridgeJSON.mount($("requestPreviewOutput"), previewResultText));
        $("requestPreviewText").textContent = previewResultText;
        setPreviewResultMode(previewResultMode);
        $("requestPreviewResult").classList.remove("hidden");
      } catch (error) { if (run === previewRun) $("requestPreviewError").textContent = error instanceof SyntaxError ? "输入请求体或标头不是合法 JSON" : error.message; }
    });
  });
  Object.values(previewInputs).forEach(fields => $(fields.input).addEventListener("input", previewInputChanged));
  $("choosePreviewDebug").addEventListener("click", () => { $("previewDebugDialog").showModal(); loadPreviewDebugRecords(); });
  $("refreshPreviewDebug").addEventListener("click", () => loadPreviewDebugRecords());
  $("previewDebugKind").addEventListener("change", () => loadPreviewDebugRecords());
  $("previewDebugSearch").addEventListener("input", () => { clearTimeout(previewDebugTimer); previewDebugTimer = setTimeout(() => { previewDebugPage = 1; loadPreviewDebugRecords(); }, 250); });
  $("previewDebugPrev").addEventListener("click", () => { if (previewDebugPage > 1) { previewDebugPage--; loadPreviewDebugRecords(); } });
  $("previewDebugNext").addEventListener("click", () => { if (previewDebugPage < previewDebugPages) { previewDebugPage++; loadPreviewDebugRecords(); } });
  $("previewDebugDialog").addEventListener("close", () => { clearTimeout(previewDebugTimer); previewDebugController?.abort(); });
  $("importOpenDebug").addEventListener("click", event => busy(event.currentTarget, () => importPreviewDebugRecord(debugExchange.id, debugPane === "request" ? "upstream_request" : debugPane === "response" ? "original_request" : debugPane)));
  $("importExecutorForPreview").addEventListener("click", event => busy(event.currentTarget, () => importPreviewDebugRecord(event.currentTarget.dataset.recordId, "executor_request")));
  $("copyPreviewResult").addEventListener("click", event => busy(event.currentTarget, async () => { await copyTextToClipboard(previewResultText); notify("预览 JSON 已复制"); }));
  $("authForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget;
    await busy(form.querySelector('[type="submit"]'), async () => {
      managementKey = $("managementKey").value.trim(); storeSessionKey(managementKey); await connect();
    });
  });
  $("keyForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget, id = $("keyId").value;
    const old = snapshot.keys.find(key => key.id === id);
    const input = { id, label: $("keyLabel").value.trim(), secret: $("apiSecret").value.trim(), base_url: $("baseUrl").value.trim(), priority: Number($("keyPriority").value), channel: $("keyChannel").value, enabled: old ? old.enabled : true };
    await busy(form.querySelector('[type="submit"]'), async () => {
      $("keyError").textContent = "";
      let key;
      try { key = await api("/keys", jsonOptions(id ? "PATCH" : "POST", input)); }
      catch (error) { $("keyError").textContent = error.message; return; }
      form.reset(); $("keyDialog").close(); notify(id ? "凭证已更新" : "凭证已添加");
      await loadState(); refreshKeyQuota(key.id, false);
    });
  });
  $("modelForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget;
    const model = { id: $("mappingId").value, client_model: $("clientModel").value.trim(), upstream_model: $("upstreamModel").value.trim(), thinking_map: $("thinkingMap").value.trim(), key_id: $("mappingKey").value, enabled: $("mappingEnabled").checked };
    await busy(form.querySelector('[type="submit"]'), async () => {
      $("modelError").textContent = "";
      try { await api("/models", jsonOptions("POST", model)); }
      catch (error) { $("modelError").textContent = error.message; return; }
      $("modelDialog").close(); await loadState(); await publishModels();
    });
  });
  $("addKey").addEventListener("click", () => openKey());
  $("addModel").addEventListener("click", () => openMapping());
  $("debugModeToggle").addEventListener("click", event => {
    const enabled = !snapshot.debug_mode;
    if (enabled && !confirm("调试模式会保存原始请求、执行器输入及完整的上游请求和响应，其中可能包含提示词及模型输出。认证信息会脱敏，最多保留最近 100 条或 256 MiB。继续启用？")) return;
    busy(event.currentTarget, async () => {
      await api("/debug", jsonOptions("PATCH", { enabled }));
      await loadState(); await loadLogs();
      notify(enabled ? "调试模式已启用" : "调试模式已关闭");
    });
  });
  $("clearDebugLogs").addEventListener("click", event => {
    if (!confirm("清除已保存的全部调试明细？普通请求日志会保留。")) return;
    busy(event.currentTarget, async () => {
      await api("/debug", { method: "DELETE" });
      await loadLogs();
      notify("调试明细已清除");
    });
  });
  $("copyDebug").addEventListener("click", event => busy(event.currentTarget, copyDebugText));
  $("keySearch").addEventListener("input", renderKeys);
  $("modelSearch").addEventListener("input", renderModels);
  $("modelStatus").addEventListener("change", renderModels);
  $("refreshQuotas").addEventListener("click", event => busy(event.currentTarget, () => refreshQuotas(true)));
  $("refreshLogs").addEventListener("click", event => busy(event.currentTarget, loadLogs));
  $("retryPublish").addEventListener("click", publishModels);
  $("syncModels").addEventListener("click", event => busy(event.currentTarget, async () => {
    const key = [...snapshot.keys].filter(item => item.enabled).sort((a, b) => (b.priority || 0) - (a.priority || 0))[0];
    if (!key) { showView("keys"); openKey(); return; }
    await api("/models/sync", jsonOptions("POST", { key_id: key.id })); await loadState();
    notify(`已拉取 ${snapshot.catalog.length} 个上游模型`);
  }));
  $("selectModels").addEventListener("change", event => {
    visibleModels().forEach(model => event.currentTarget.checked ? selectedModels.add(model.id) : selectedModels.delete(model.id)); renderModels();
  });
  for (const [id, enabled] of [["enableSelected", true], ["disableSelected", false]]) {
    $(id).addEventListener("click", event => busy(event.currentTarget, () => modelMutation("/models", jsonOptions("PATCH", { ids: [...selectedModels], enabled }))));
  }
  const reloadFirstPage = () => { page = 1; loadLogs().catch(showError); };
  $("logSearch").addEventListener("input", () => { clearTimeout(searchTimer); searchTimer = setTimeout(reloadFirstPage, 250); });
  ["logStatus", "logModel", "logKey", "logThinking", "pageSize"].forEach(id => $(id).addEventListener("change", reloadFirstPage));
  $("applyRange").addEventListener("click", reloadFirstPage);
  $("prevPage").addEventListener("click", () => { if (page > 1) { page--; loadLogs().catch(showError); } });
  $("nextPage").addEventListener("click", () => { if (page < totalPages) { page++; loadLogs().catch(showError); } });
  document.querySelector(".plugin-nav").addEventListener("keydown", event => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const tabs = [...document.querySelectorAll("[data-view]")];
    let index = tabs.indexOf(document.activeElement);
    if (event.key === "Home") index = 0;
    else if (event.key === "End") index = tabs.length - 1;
    else index = (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
    tabs[index].focus(); showView(tabs[index].dataset.view);
  });
  document.body.addEventListener("change", event => {
    const checkbox = event.target.closest("[data-select-model]");
    if (checkbox) { checkbox.checked ? selectedModels.add(checkbox.dataset.selectModel) : selectedModels.delete(checkbox.dataset.selectModel); updateSelection(); }
  });
  $("keyList").addEventListener("keydown", event => {
    const card = event.target.closest(".quota-pool[data-channel-key]");
    if (!card || (event.key !== "Enter" && event.key !== " ")) return;
    event.preventDefault();
    if (card.getAttribute("aria-disabled") !== "true") switchKeyChannel(card.dataset.channelKey, card.dataset.channel);
  });
  document.body.addEventListener("click", event => {
    const button = event.target.closest("button, .quota-pool[data-channel-key]");
    if (!button || button.disabled || button.getAttribute("aria-disabled") === "true") return;
    const data = button.dataset;
    if (data.close) $(data.close).close();
    if (data.view) showView(data.view);
    if (data.debugLog) busy(button, () => openDebugLog(data.debugLog));
    if (data.debugPane) { debugPane = data.debugPane; renderDebugPane(); }
    if (data.range) {
      range = data.range;
      document.querySelectorAll("[data-range]").forEach(item => item.setAttribute("aria-pressed", String(item.dataset.range === range)));
      $("customRange").classList.toggle("hidden", range !== "custom");
      if (range !== "custom") reloadFirstPage();
    }
    if (data.editKey) openKey(snapshot.keys.find(key => key.id === data.editKey));
    if (data.quotaKey) refreshKeyQuota(data.quotaKey, true);
    if (data.channelKey) switchKeyChannel(data.channelKey, data.channel);
    if (data.testKey) busy(button, async () => { const result = await api("/keys/test", jsonOptions("POST", { id: data.testKey })); notify(`连接成功，可用 ${result.model_count} 个模型`); });
    if (data.toggleKey) busy(button, async () => {
      const key = snapshot.keys.find(key => key.id === data.toggleKey);
      await api("/keys/toggle", jsonOptions("PATCH", { id: key.id, enabled: !key.enabled })); await loadState();
    });
    if (data.deleteKey && confirm("删除此凭证？绑定它的模型将改为自动选择凭证。")) busy(button, async () => { await api("/keys/delete", jsonOptions("DELETE", { id: data.deleteKey })); await loadState(); notify("凭证已删除"); });
    if (data.editModel) openMapping(snapshot.models.find(model => model.id === data.editModel));
    if (data.toggleModel) busy(button, () => {
      const model = snapshot.models.find(item => item.id === data.toggleModel);
      return modelMutation("/models", jsonOptions("PATCH", { ids: [model.id], enabled: !model.enabled }));
    });
    if (data.deleteModel && confirm("删除此模型映射并从 CPA 模型列表移除？")) busy(button, () => modelMutation("/models/delete", jsonOptions("DELETE", { id: data.deleteModel })));
    if (data.ruleEdit) openRequestRule(snapshot.request_edit.rules.find(rule => rule.id === data.ruleEdit));
    if (data.ruleToggle) busy(button, () => changeRequestRule(data.ruleToggle, "toggle"));
    if (data.ruleUp) busy(button, () => changeRequestRule(data.ruleUp, "up"));
    if (data.ruleDown) busy(button, () => changeRequestRule(data.ruleDown, "down"));
    if (data.ruleDelete && confirm("删除此请求编辑规则？")) busy(button, () => changeRequestRule(data.ruleDelete, "delete"));
    if (data.previewImport) busy(button, async () => {
      $("previewDebugError").textContent = "";
      try { await importPreviewDebugRecord(data.previewImport, data.previewKind); }
      catch (error) { $("previewDebugError").textContent = error.message; }
    });
    if (data.jsonInput) {
      try { setPreviewInputMode(data.jsonInput, data.jsonMode); $("requestPreviewError").textContent = ""; }
      catch (error) { $("requestPreviewError").textContent = error instanceof SyntaxError ? "请先修正输入 JSON，再切换折叠视图" : error.message; }
    }
    if (data.formatJson) {
      try {
        const fields = previewInputs[data.formatJson];
        $(fields.input).value = BridgeJSON.format($(fields.input).value);
        if (!$(fields.structured).classList.contains("hidden")) setPreviewInputMode(data.formatJson, "tree");
        $("requestPreviewError").textContent = "";
      } catch (error) { $("requestPreviewError").textContent = error instanceof SyntaxError ? "输入 JSON 无效，无法格式化" : error.message; }
    }
    if (data.jsonOutputMode) setPreviewResultMode(data.jsonOutputMode);
    if (data.jsonExpand) jsonViews.get(data.jsonExpand)?.expandAll();
    if (data.jsonCollapse) jsonViews.get(data.jsonCollapse)?.collapseAll();
  });
  if (managementKey) connect(); else showAuth();
})();
