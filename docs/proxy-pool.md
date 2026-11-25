# 凭证代理池

在插件“代理设置”中添加命名代理，再到“凭证管理 → 编辑 → 凭证代理”选择。每个代理可供多个凭证复用；支持 HTTP、HTTPS、SOCKS5、SOCKS5H 及 URL 用户名密码。

## 生效顺序

1. 凭证独立代理：插件代理池绑定、独立直连，或 CPA 原生凭证中的 `proxy_url`。
2. 插件默认代理：保留原来的 `proxy_url` 设置，仅用于没有独立配置的凭证。
3. CPA 全局出口策略：前两项为空时使用 CPA HTTP 回调。

“独立直连”及 CPA 的 `direct`／`none` 会明确绕过代理，包括环境变量中的代理。“默认代理 / CPA 全局配置”会清除该凭证的独立覆盖，先采用插件默认值。要让全部凭证直接沿用 CPA 全局策略，清空插件默认代理。

聊天、流式、模型目录、连接测试、原生额度和插件额度查询都使用同一优先级。代理连接失败直接报错，不换到其他出口。请求读取配置快照，正在进行的流不会因编辑代理而被关闭。传输连接按代理地址独立复用，最多保留 64 个 transport。

## 与 CPA 协调

- 池绑定和直连会同步到 CPA 原生凭证 JSON 的 `proxy_url`。插件附带 `bridge_proxy_mode`、`bridge_proxy_id`、`bridge_proxy_url` 记录该值是否仍由插件维护。
- CPA 的值仍等于上次同步值时，修改池地址会更新绑定它的原生凭证。
- 在 CPA 手动改地址、切换直连或清空覆盖后，插件以 CPA 当前值为准，显示“CPA 独立代理”或默认配置。普通凭证编辑、池编辑和重启不会覆盖这个修改。
- 如需重新交给代理池管理，在插件凭证编辑中显式重新选择池；改为默认或直连也会同步到 CPA。
- 只编辑名称、渠道、优先级的客户端未提交代理字段时，保留已有选择。
- 仍被插件配置绑定的代理不能删除，即使该凭证当前已在 CPA 覆盖了代理。请先将该凭证显式改为默认、直连或另一个代理，再删除旧池项。
- 同步失败返回错误，插件状态可能已保存。请重试相同操作；现有 CPA 覆盖仍保留。CPA 文件与插件状态不支持跨文件事务。

CPA 的动态库 HTTP 回调没有可依赖的凭证代理参数。因此凭证独立代理由插件 HTTP Transport 执行，未指定独立代理及插件默认代理时继续走 CPA HTTP 回调。独立传输保留插件日志和调试明细，但不经过 CPA 的 HTTP 回调日志。

## 数据与管理接口

`state.json` 的 `proxies` 数组保存 `{id,name,url}`，Key 的 `proxy_mode` 为 `inherit`／`pool`／`direct`，`proxy_id` 保存池引用。这些字段随原子状态写入一起保存。CPA 中的原生自定义覆盖不会复制到插件池。

| 管理 API | 用途 |
| --- | --- |
| `POST /plugins/mistral-bridge/proxies` | 添加 `{name,url}` |
| `PATCH /plugins/mistral-bridge/proxies` | 编辑 `{id,name,url}` |
| `DELETE /plugins/mistral-bridge/proxies` | 删除 `{id}`，被绑定时返回 409 |
| `POST/PATCH /plugins/mistral-bridge/keys` | 可选 `proxy_mode`、`proxy_id`；省略保持现有设置 |
| `PATCH /plugins/mistral-bridge/proxy` | 修改原有默认代理 |

API 均使用 CPA 管理鉴权。完整代理 URL 在管理接口和编辑表单中可见，并可能在 CPA 引用文件中包含认证信息；列表及凭证摘要遮蔽 URL 用户名密码。数据目录与 CPA auth 目录均应按凭证目录保管。

## 验证

Go 测试覆盖并发隔离、代理优先级、CPA 所有权交接、持久化及失败回滚、直连、连接复用、失败不切换出口。`scripts/check_proxy_pool_cpa.py` 在隔离 CPA v8.0.78 中验证不同凭证的普通／流式请求、目录查询、CPA 修改、重启与删除；`scripts/check_native_quota_cpa.py` 用本地 TLS 计费模拟服务验证双池额度查询遵循凭证代理。所有测试仅使用模拟凭据。
