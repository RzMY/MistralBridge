# 模型限额与冷却

限额表位于 `rate_limits.json`，编译时内嵌，来源为 `docs/Rate limits - Admin - Mistral.html`（2026-11-25 下载的 EU 账号页面）。这是该页面的账号限额快照，并非所有 Mistral 套餐的统一限额。

## CPA 原生调度

插件声明 `auth_provider`、`model_provider` 和 `executor_model_scope="oauth"`（这是 CPA 的凭证执行路径名称，Mistral 仍使用 API Key）。每个 Key 对应独立的 CPA auth；模型通过 `model.for_auth` 按凭证发布。插件不再声明 `model_router`，也不再自行轮询、回退或保存上游 429 冷却。

- 凭证选择、权重、优先级、启停、前缀、重试和错误冷却由 CPA 管理。执行器必须收到 CPA 选定的 `AuthID`、`AuthProvider` 和凭证数据；缺失或失效时返回错误，不切换到其他 Key。
- 显式绑定的模型只发布给绑定凭证；其他模型可发布给全部凭证。同名模型可与其他提供商共用 CPA 路由，支持 CPA 原生失败回退。
- HTTP 429 通过错误信封的 `http_status=429`、`code=rate_limited` 和可选 `retry_after_seconds` 报告。支持上游 `Retry-After` 秒数、小数秒、HTTP 日期；无有效提示时使用 CPA 的默认策略。CPA 可对上游提示应用自己的下限或限制。
- 原生冷却重置与 `disable_cooling` 生效，插件没有额外的上游错误倒计时。503 等失败也进入 CPA 的自定义冷却策略。已接受的流中断使用 `error_detail.code=stream_truncated`，避免重放已经输出的响应。

## 模型预算

- 内置 TPM/RPS 表继续作为发送前的限额检查，按 API 地址、凭证和上游模型记账；同一模型的客户端映射、不同渠道共用预算。检查只作用于 CPA 已选定的 Key。
- RPS 使用最小发送间隔 `1 / RPS`，例如 0.5 RPS 每 2 秒最多发送一次，不允许瞬时突发。检查与预留在同一锁内完成。
- TPM 预留输入估算加 `max_tokens`，输入估算为消息及工具 JSON 的 UTF-8 字节数 / 3 加 16，未提供输出上限时预留 4096。单次预留最多占满一分钟额度；超大请求由上游最终判定。
- 普通响应和 SSE usage 校正实际 Token。无 usage、传输失败或流中断保留估算；明确 HTTP 4xx/5xx 释放预留。进行中的请求持续占用预留，完成一分钟后释放。
- 限额检查拒绝发送时返回 `transient_rate_limit` 和精确到整秒的剩余等待时间。CPA 将其作为模型临时冷却处理，而非账号额度耗尽；随后可选择其他凭证或提供商。原生冷却重置不会虚增尚未恢复的 TPM/RPS 预算。
- 表按上游模型 ID 精确匹配，不猜测移动的 `-latest` 别名。表外模型没有预设 TPM/RPS，但上游错误仍交给 CPA 冷却。

本地预算在插件重启后清空；CPA 可自行持久化错误冷却。不同 Key 仍独立记账，无法预知同组织其他 Key 或其他应用的用量，以上游反馈为准。迁移、配置和原生凭证文件见 [CPA 原生接入](native-cpa.md)。

## 内置表

| 上游模型 | TPM | RPS |
| --- | ---: | ---: |
| `codestral-2508` | 2,000,000 | 16.67 |
| `codestral-embed` | 5,000,000 | 12.00 |
| `deepseek-v4-1-flash-1017` | 10,000,000 | 22.22 |
| `glm-5-2` | 300,000 | 0.50 |
| `ministral-14b-2512` | 3,000,000 | 4.00 |
| `ministral-3b-2512` | 4,000,000 | 100.00 |
| `ministral-8b-2512` | 2,000,000 | 25.00 |
| `mistral-embed-2312` | 20,000,000 | 12.00 |
| `mistral-large-2512` | 800,000 | 0.50 |
| `mistral-large-4` | 15,000,000 | 33.33 |
| `mistral-medium-latest` | 1,000,000 | 33.33 |
| `mistral-moderation-2603` | 5,000,000 | 16.67 |
| `mistral-small-2603` | 500,000 | 16.67 |
| `voxtral-mini-2602` | 2,000,000 | 12.00 |
| `voxtral-mini-transcribe-3` | 5,000,000 | 12.00 |
| `voxtral-mini-transcribe-realtime-2602` | 1,200,000 | 12.00 |
| `voxtral-mini-transcribe-realtime-3` | 5,000,000 | 12.00 |
| `voxtral-mini-tts-2603` | 5,000,000 | 12.00 |
| `voxtral-mini-tts-3-0` | 5,000,000 | 12.00 |
| `voxtral-small-2507` | 400,000 | 12.00 |
| `voxtral-small-transcribe-3` | 5,000,000 | 12.00 |
| `zai-glm-5-3` | 32,000,000 | 66.67 |
| `zai-glm-5-3-flash` | 10,666,666 | 22.22 |

## 验证

`go test -race -count=1 ./...` 检查凭证选择身份、绑定、原子预算、TPM 校正、长流预留、错误信封、迁移及原生元数据保留。

`scripts/check_native_scheduler_cpa.py` 在独立 CPA v8.1.2 容器中使用回环模拟接口和 `mock-*` 凭据，验证 3:1 加权分配、普通/流式 429、原生冷却重置、2 秒自定义冷却、关闭冷却、TPM/RPS、跨提供商回退、重启与原生启停。脚本不接受真实凭证参数，结束后删除测试容器，保留报告和日志。

```sh
python3 scripts/check_native_scheduler_cpa.py --library build/mistral-bridge.so --work-dir /tmp/mistralbridge-native-test
```
