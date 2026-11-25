# CPA 原生接入

实现和测试依据 CPA v8.1.2 的 [插件能力](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/sdk/pluginapi/types.go)、[错误信封](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/sdk/pluginabi/types.go)、[凭证保存回调](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/examples/plugin/host-callback-auth-files/README.md) 和 [管理 API](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/docs/management-api-v8.md)。建议使用 CPA v8.1.2 或更新版本；更旧版本未作兼容保证。

## 自动迁移

首次模型注册时，为现有每个 Key 调用 `host.auth.save`，生成 `<auth-dir>/mistral-bridge-<稳定ID摘要>.json`。文件包含 `type=mistral-bridge`、`bridge_managed=true`、`bridge_key_id`、标签、优先级和启停状态，不复制真实 API Key；真实 Key 继续位于插件 `state.json`。两个目录都需持久化。

凭证类型通过 `Attributes.auth_kind=apikey` 声明，不向 `api_key` 属性写入占位值，避免 CPA 将内部标记作为 `account` 返回、覆盖面板中的凭证名称。

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

## 原生凭证额度

插件声明 CPA 的 `quota_provider` 能力，并实现 `quota.identifier`、`quota.describe`、`quota.fetch`。原生凭证列表返回 `supports_quota=true` 和 `quota_provider=mistral-bridge`，支持该标准接口的 CPA 管理页会显示额度刷新入口。

- Vibe 和 Studio / API 分组展示，当前使用渠道排在前面并有文字标识。
- 返回套餐、剩余比例、重置时间，以及两个池的剩余／已用／月度金额。按量付费状态和数据时间放在额度说明中。
- 与插件页面共用按 Key 的五分钟缓存、并发合并和历史值持久化；插件页面强制刷新后，CPA 原生入口立即读取更新后的结果。CPA 的标准额度 RPC 不提供强制刷新参数。
- 单池失败保留该池历史值及原时间，显式标注“历史数据”和刷新错误；缺少历史数据时不伪造零余额。全部池不可用且无历史值时返回错误。
- 禁用的凭证仍可查看额度。直接导入 CPA 的独立 Mistral 凭证也支持查询，使用自身 Key；其结果不写入插件管理的 Key 列表。
- 自定义 API 地址仍不查询 Mistral 官方计费接口。额度由 Mistral 管理，不提供额度重置操作；CPA 的路由冷却重置是另一个独立功能。

可通过 `POST /v0/management/quota/fetch`，或 `POST /v0/management/plugins/mistral-bridge/quota`，提交 `{"auth_index":"CPA凭证索引"}` 获取标准额度响应。描述接口为 `GET /v0/management/quota/providers`。额度接口沿用 CPA 管理鉴权，不需要提交上游 Key。

2026-11-25 实测远端 `8317` 端口的 CPA 自带管理页支持上述通用插件额度显示；该机器 `18317` 端口的 CPAMP 版本没有消费 `supports_quota`／`quota_provider` 通用接口，需其面板提供支持后才可在 CPAMP 凭证卡片中显示。插件额度接口不要求替换 CPA 或 CPAMP。

`scripts/check_native_quota_cpa.py` 在隔离 CPA v8.0.78 中使用临时 CA、本地 TLS 模拟代理和 `mock-*` 凭据，验证能力发现、双池归一化、缓存共享、刷新、部分失败、停用凭证和独立导入。代理不会建立外部连接，不读取正式环境凭据。

## 同步故障

插件状态与 CPA 原生凭证是两个存储。同步失败返回 HTTP 503 和明确错误，保留已保存的插件状态；请重试相同保存或模型同步。缺失或已删除的选中凭证会拒绝请求，执行器不会自行换 Key。引用文件可通过重新打开插件或重新发布模型修复；涉及已有原生策略的失败修改应重试相同保存操作。
