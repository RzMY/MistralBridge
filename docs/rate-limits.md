# 模型限额与冷却

限额表位于 `rate_limits.json`，编译时内嵌，来源为 `docs/Rate limits - Admin - Mistral.html`（2026-11-25 下载的 EU 账号页面）。这是该页面的账号限额快照，并非所有 Mistral 套餐的统一限额。

## 调度行为

- 按 API 地址、凭证和上游模型分别记账；同一上游模型的多个客户端映射、不同使用渠道共用预算。显式绑定的映射只使用绑定 Key。
- 在未冷却的候选中优先选最高优先级，同级轮询；高优先级 Key 受限时可选低优先级 Key。全部受限时立即返回 HTTP 429，错误消息包含最早可重试的秒数，不发送上游请求，也不在插件内循环重试。收到上游 429 的当次请求返回错误，后续请求重新调度。
- RPS 使用最小发送间隔 `1 / RPS`，例如 0.5 RPS 每 2 秒最多发送一次，不允许瞬时突发。选择与预算预留在同一互斥锁下完成，防止并发越限。
- TPM 在发送前预留输入估算值加 `max_tokens`。输入按消息及工具 JSON 的 UTF-8 字节数 / 3 加 16 估算；未提供输出上限时预留 4096。单次预留最多占满该模型一分钟额度，因此超大请求仍会由上游作最终判定。
- 普通响应和流式 SSE 的 usage 都会校正实际输入＋输出 Token；无 usage、传输失败或流中断时保留估算；明确收到 HTTP 4xx/5xx 拒绝时释放 Token 预留，RPS 间隔及 429 冷却仍保留。进行中的请求持续占用预留，完成后保留一分钟再释放，长流不会在一分钟后提前释放。这一策略较保守，尤其是长请求或未提供输出上限的请求。
- 上游 HTTP 429 立即进入冷却，支持 `Retry-After` 秒数、小数秒及 HTTP 日期。缺失、过期或无效时按 60、120、240、480、900 秒退避；冷却结束后超过 5 分钟未再次受限则重置退避。并发返回的成功响应不会清除已触发的冷却。
- 限额表按上游模型 ID 精确匹配，不猜测移动的 `-latest` 别名（表中的 `mistral-medium-latest` 除外）。未知模型仍有 429 冷却，但没有预设 TPM/RPS 限制。

状态仅在本插件进程内保存，重启后清空。当前凭证数据没有组织 ID，因此不同 Key 独立记账；同组织的多个 Key、其他应用和其他插件进程可能共享上游限额，本地估算无法覆盖这些外部用量，仍以上游 429 为准。CPA 的当前错误信封仅传递状态码和消息，剩余等待时间通过错误消息返回。

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

`go test -race -count=1 ./...` 覆盖原子预留、优先级回退、显式绑定、映射共享、模型隔离、低于 1 的 RPS、TPM 实际用量校正、长流预留、429 退避和到期恢复。测试时钟可控，无需访问 Mistral。

远程 CPA 集成测试使用 `scripts/check_rate_limits_cpa.py`，只接受构建产物与独立工作目录，不加载现有 CPA 配置或任何真实凭证。脚本启动绑定回环地址的模拟服务和独立 Docker CPA，检查普通/流式 429、Retry-After、默认冷却、TPM、RPS、回退和恢复，最后删除测试容器，保留日志及 `report.json`。

```sh
python3 scripts/check_rate_limits_cpa.py \
  --library build/mistral-bridge.so \
  --image eceasy/cli-proxy-api:v8.1.2 \
  --work-dir /tmp/mistralbridge-rate-limits-test
```
