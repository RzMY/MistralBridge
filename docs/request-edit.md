# 请求编辑

在控制台的 **请求编辑** 页面配置输入标头和请求体。配置保存后立即用于新请求，普通与流式请求共用同一流程。旧版本的状态无需手动迁移，**内置字段映射**和**自动丢弃不兼容字段**默认开启，自定义规则列表为空。

## 内置字段对照

以下字段对照依据 2026-11-24 阅读的官方 [Mistral Chat API](https://docs.mistral.ai/api/endpoint/chat)、[Mistral OpenAPI schema](https://docs.mistral.ai/openapi.yaml)、[OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)、[OpenAI Responses](https://developers.openai.com/api/reference/resources/responses/methods/create)、[Claude Messages](https://platform.claude.com/docs/en/api/messages/create) 和 [Gemini GenerationConfig](https://ai.google.dev/api/generate-content) 实现。

| 输入字段 / 形式 | Mistral 原生字段 / 行为 |
| --- | --- |
| `max_completion_tokens`（OpenAI Chat）、`max_output_tokens`（Responses）、`maxOutputTokens` | `max_tokens` |
| `seed`（OpenAI / Gemini） | `random_seed` |
| `stop_sequences`（Claude）、`stopSequences`（Gemini） | `stop` |
| `topP`、`topK` | `top_p`、`top_k` |
| `reasoning.effort` | `reasoning_effort`；去掉仅影响 Responses 输出的 `reasoning.summary` |
| `generationConfig.maxOutputTokens / temperature / topP / topK / stopSequences / seed / candidateCount` | `max_tokens / temperature / top_p / top_k / stop / random_seed / n` |
| `messages[].role="developer"` | `system` |
| assistant 消息的 `reasoning_content / reasoning / reasoning_details` | Mistral `thinking` 内容块，文本与工具调用保留 |
| 顶层 `store / stream_options / user / logit_bias / prompt_cache_retention / safety_identifier` | 移除：不在 Mistral 原生 Chat 请求 schema 中 |
| 消息内容块 `cache_control`、非 tool 消息 `name`、assistant 消息 `refusal` | 移除：不在对应 Mistral 消息 schema 中 |

先保留已有非 `null` 原生值，再按表中的别名顺序取第一个非 `null` 值；`generationConfig` 的采样值最后补充。处理后移除已映射的来源字段，空的配置对象移除，其他字段保留。原生 `response_format`、`tools`、`parallel_tool_calls`、`logprobs`、`safe_prompt` 等不改写；工具参数 schema 和 metadata 中的同名字段也不改写。

这些映射对齐兼容的生成参数。完整 Claude、Responses、Gemini 消息协议由 CPA 转换，规则处理的是 CPA 交给执行器的 Chat 请求体。例如 Responses 请求应使用转换后的 `/messages` 路径；CPA 未传递的原始扩展字段不能作为规则来源，可通过设置值或输入标头补充。思考预算、供应商服务等级等没有直接等价语义的字段不会推测转换。

关闭 **内置字段映射** 会停止上述兼容转换，自定义规则和自动丢弃开关仍独立生效。模型路由、模型配置中的思考强度映射、使用渠道和鉴权仍按插件配置应用。

## 自动丢弃不兼容字段

**自动丢弃不兼容字段**默认开启。在内置映射和全部自定义规则之后，按照内置的官方 Mistral 请求 schema 移除仍然存在的多余 JSON 字段，减少 `extra_forbidden` 导致的 HTTP 422。自定义规则可以先把客户端扩展字段移动或复制到 Mistral 支持的位置，例如 `/metadata/client_tag`；复制后留在原位置的不兼容字段仍会丢弃。

检查范围包括请求顶层、各角色消息、已知内容块及多模态子对象、工具定义和调用外壳、`tool_choice`、`response_format` 等明确声明 `additionalProperties: false` 的对象。metadata、工具的 `parameters`、函数调用的 `arguments` 和输出 JSON Schema 内的任意业务字段保留。未知角色或内容类型、已知字段的值类型和取值范围交由上游校验，不会猜测修改或删除完整消息。

此开关独立于内置映射。关闭内置映射但保持自动丢弃开启时，`seed` 等没有被转换的非原生字段也会被丢弃；自定义上游需要扩展字段时，可关闭自动丢弃开关。

请求日志在结果列显示 **自动丢弃 N 项**，展开可查看该步骤删除的字段路径、原因和值摘要，支持按路径或值搜索。成功、失败、普通和流式请求均记录，无需开启调试模式。预览也返回相同明细。已有内置映射中的删除和用户主动配置的删除不计入此步骤。

丢弃内容中的密钥类字段、当前上游 Key 和输入认证标头值会脱敏。每条日志最多保留 64 项明细、每个值最多约 2 KiB，整体摘要约 16 KiB；超限时保留实际总数并标记截断或未展示数量。长文本按 UTF-8 边界截断，整数值按原精度显示。

字段定义来自 [Mistral 官方 OpenAPI](https://docs.mistral.ai/openapi.yaml)，作为 `request_schema.json` 编译进插件，请求处理时不联网。维护者可安装 PyYAML 后运行 `python scripts/update_request_schema.py` 更新字段投影；文件记录来源、读取日期和原始 schema 的 SHA-256。

## 自定义规则

点击 **添加规则**，填写名称、操作、来源与目标位置，然后保存。支持四种操作：

- **设置值**：填写 JSON 值。请求体支持数字、布尔值、字符串、数组、对象和 `null`；标头支持 JSON 字符串或字符串数组。
- **复制字段**：保留来源，将其值写到目标。支持请求体与标头之间复制。请求体复制到标头时，来源值须为字符串或字符串数组。
- **移动 / 重命名字段**：目标成功写入后移除来源；未覆盖已有目标时保留来源。
- **删除字段**：移除目标；目标不存在时跳过。

规则在内置映射之后按列表从上到下执行，支持上移、下移、编辑、删除和单条启停。**覆盖现有目标值**关闭时，已有目标（包括 `null`）保留。标头的目标指已明确写入的上游标头；输入标头只作为来源，未经规则写入的值不转发。

请求体字段使用 JSON Pointer，标头字段直接填写名称，标头名称大小写不敏感。

| 目标路径 | 含义 |
| --- | --- |
| `/temperature` | 顶层温度 |
| `/metadata/project` | metadata 的 project 字段；缺失的对象会创建 |
| `/messages/0/content` | 第一条消息的内容 |
| `/messages/*/content/*/cache_control` | 每条消息内容数组中的缓存控制字段 |
| `/metadata/items/*/old` → `/metadata/items/*/new` | 对数组成员逐个重命名字段 |
| `/metadata/a~1b~0c` | metadata 中名称为 `a/b~c` 的字段 |

`*` 匹配现有数组或对象成员，复制与移动的来源、目标通配符数量须相同，按出现顺序对应。数组删除从后向前执行，避免移动索引。每条规则最多匹配 4096 个字段，索引范围为 0–4095；最多保存 100 条规则，路径最多 32 层，设置值最多 64 KiB。

`model`、`stream`、Authorization、Host、Content-Type、Accept 和 HTTP 传输标头由插件管理，规则不能改写。请求字段编辑结束后，再应用上游模型、思考强度映射、凭证、使用渠道和请求模式。路径的父字段类型错误或向标头写入非字符串值时，请求返回 HTTP 400，错误包含规则序号与名称。

## 预览

展开 **预览请求编辑**，输入 JSON 请求体与标头对象，点击 **预览编辑结果**。预览使用已保存的配置，不发送 Mistral 请求。结果显示字段编辑后的请求体与待转发标头；实际请求还会应用模型映射、凭证和使用渠道。保存新配置后旧预览会隐藏，重新预览即可查看新结果。

例如，添加一条来源为输入标头 `X-Client-Tag`、目标为请求体 `/metadata/client_tag` 的复制规则后，预览输入 `{"X-Client-Tag":"desktop"}` 会得到 `metadata.client_tag="desktop"`。

## 管理 API

鉴权复用 CPA 管理 API，路径前缀为 `/v0/management/plugins/mistral-bridge`。

| 方法与路径 | 行为 |
| --- | --- |
| `GET /state` | 返回 `request_edit: {builtin_enabled, auto_drop_incompatible, rules}` |
| `PATCH /request-edit` | 独立更新任一开关，或替换整个有序规则列表 |
| `POST /request-edit/preview` | 接收 `body` JSON 对象与 `headers` 对象，返回编辑结果及 `dropped_count`、`dropped_fields` |
| `GET /logs` | 日志中的 `dropped_count`、`dropped_fields` 表示本次自动清理，明细含 `path`、`value`、`reason`、`truncated` |

关闭内置映射且保留已有规则：

```json
{"builtin_enabled": false}
```

关闭自动丢弃且保留内置映射与已有规则：

```json
{"auto_drop_incompatible": false}
```

替换自定义规则列表且保留内置开关：

```json
{
  "rules": [
    {
      "name": "设置温度",
      "enabled": true,
      "action": "set",
      "target": "body",
      "path": "/temperature",
      "value_json": "0.3",
      "overwrite": true
    },
    {
      "name": "复制输入标记",
      "enabled": true,
      "action": "copy",
      "source": "header",
      "from": "X-Client-Tag",
      "target": "body",
      "path": "/metadata/client_tag",
      "overwrite": false
    }
  ]
}
```

设置值以 `value_json` 字符串提交和保存，字符串内容是 JSON 字面量；例如 JSON 字符串值 `example` 对应 `"value_json":"\"example\""`。这样规则保存、页面编辑和预览不会把 `9007199254740993` 等整数舍入。新增规则未提供 ID 时由服务器分配，编辑与排序时保留 ID。`{"rules":[]}` 清空自定义规则。

配置位于 `<auth-dir>/plugin-data/mistral-bridge/state.json` 的 `request_edit` 字段中，使用原子写入；校验或保存失败会保留原配置。开启请求调试后，明细记录最终编辑后的实际上游请求。
