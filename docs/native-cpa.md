# CPA 原生接入

实现和测试依据 CPA v8.1.2 的 [插件能力](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/sdk/pluginapi/types.go)、[错误信封](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/sdk/pluginabi/types.go)、[凭证保存回调](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/examples/plugin/host-callback-auth-files/README.md) 和 [管理 API](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/docs/management-api-v8.md)。建议使用 CPA v8.1.2 或更新版本；更旧版本未作兼容保证。

## 自动迁移

首次模型注册时，为现有每个 Key 调用 `host.auth.save`，生成 `<auth-dir>/mistral-bridge-<稳定ID摘要>.json`。文件包含 `type=mistral-bridge`、`bridge_managed=true`、`bridge_key_id`、标签、优先级和启停状态，不复制真实 API Key；真实 Key 继续位于插件 `state.json`。两个目录都需持久化。

已有原生文件中的 `weight`、`disable_cooling`、`request_retry`、`prefix`、`proxy_url` 及未知扩展字段均保留，重启不会恢复成插件默认值。插件页面显式修改优先级或启停时同步相应字段。Key 删除时先经 CPA 禁用对应凭证，再删除属于该 Key 的引用文件，由 CPA 文件监听器移除记录。直接删除 CPA 引用文件会在插件下一次同步时重建；永久移除请从插件删除 Key。

模型绑定通过每凭证模型列表生效。插件不再绕过 CPA 模型解析；与其他提供商使用相同客户端模型名时，可参与共同路由及失败回退。

## 在 CPA 配置调度

在 CPA 原生凭证列表中可看到每个 `mistral-bridge` 凭证，可设置权重、优先级、禁用状态等，或编辑对应凭证 JSON。例如：

```json
{
  "type": "mistral-bridge",
  "bridge_managed": true,
  "bridge_key_id": "已有插件Key的ID",
  "weight": 3,
  "priority": 0,
  "disable_cooling": false,
  "request_retry": 0
}
```

上例仅说明字段，修改已有文件时保留其他字段。全局策略仍在 CPA 配置中：

```yaml
routing:
  strategy: weighted-round-robin
  retry:
    request-retry: 0
    max-retry-interval: 0
  cooldown:
    disable-cooling: false
    transient-error-cooldown-seconds: 2
    save-cooldown-status: true
```

`weight` 默认为 1，CPA 加权策略下小于等于 0 不参与选择；权重只影响 CPA 判定可选的同级凭证。`transient-error-cooldown-seconds` 用于 503 等临时错误，429 则使用 CPA 的限流策略及上游重试提示。通过 CPA 原生冷却重置入口可以立即清除上游错误冷却。插件 TPM/RPS 预算单独约束实际发送速率，详见 [限额说明](rate-limits.md)。

## 独立凭证导入

也可以直接向 CPA 导入独立 JSON：

```json
{
  "type": "mistral-bridge",
  "label": "Mistral API",
  "api_key": "YOUR_MISTRAL_KEY",
  "mistral_base_url": "https://api.mistral.ai/v1",
  "channel": "studio",
  "weight": 1
}
```

独立凭证使用插件中未绑定 Key 的模型映射。请使用 `mistral_base_url`，不添加 `compat_name`；不要将同一个 `mistral-bridge` provider 同时配置成 OpenAI-compatible，否则 CPA 会优先采用其原生兼容执行器。独立导入凭证由 CPA 管理，不出现在插件自己的 Key/额度列表中。

## 同步故障

插件状态与 CPA 原生凭证是两个存储。同步失败返回 HTTP 503 和明确错误，保留已保存的插件状态；请重试相同保存或模型同步。缺失或已删除的选中凭证会拒绝请求，执行器不会自行换 Key。引用文件可通过重新打开插件或重新发布模型修复；涉及已有原生策略的失败修改应重试相同保存操作。
