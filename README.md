<div align="center">

<img src="logo.png" alt="Mistral Bridge" width="96" height="96">

# Mistral Bridge

将 Mistral 接入 CPA，在 OpenAI 兼容客户端中使用。

[![Build](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml/badge.svg)](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platforms](https://img.shields.io/badge/Linux%20%7C%20macOS%20%7C%20Windows-64748b)](https://github.com/RzMY/MistralBridge/releases)

**简体中文** · [English](README_EN.md)

[下载版本](https://github.com/RzMY/MistralBridge/releases) · [构建产物](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml) · [反馈问题](https://github.com/RzMY/MistralBridge/issues)

</div>

Mistral Bridge 是 CLIProxyAPI（CPA）的原生动态库插件。它对齐 Mistral 与 OpenAI Chat Completions 的请求、响应和思考字段，并提供凭证、模型映射和请求日志管理页面。协议转换参考 magpie 的公开实现。[CPA](https://github.com/router-for-me/CLIProxyAPI) · [magpie](https://github.com/yetone/magpie)

开发和维护说明见[技术设计文档](docs/technical-design.md)。

## 功能

- **协议适配**。转换 assistant 思考字段、Mistral thinking/text 内容及流式增量响应，支持工具调用消息中的思考内容。
- **凭证管理**。添加、编辑和测试多个 Key，查看 API 或 Vibe 月度额度，设置启用状态与调度优先级。
- **模型映射**。拉取上游目录，自定义客户端模型名和思考强度映射。启用、禁用或修改映射后同步到 CPA 模型列表。
- **请求日志**。按模型、结果、凭证、思考强度和时间范围筛选，汇总请求数、Token、缓存命中率及生成速率。
- **请求调试**。启用后保存发往 Mistral 的完整请求与响应，包括流式内容和错误正文。认证信息脱敏，独立保留最近 100 条，可从请求日志查看或清理。
- **面板集成**。复用 CPA / CPAMP 保存的同源管理会话，跟随 CPAMP 切换深色与浅色主题。上游请求、模型拉取及额度查询使用 CPA 的 HTTP 出口和代理配置。

## 界面预览

![请求日志](imgs/logs.png)
![凭证管理](imgs/keys.png)
![模型映射](imgs/models.png)

## 安装与配置

需要支持 C ABI v1 和插件 schema version 10 的 CPA。已在 CPA `v8.0.78` 与 CPAMP 中验证。

### 在线安装

在 CPA 插件市场的 CLIProxyAPI源（官方源） 中安装 Mistral Bridge。

### 手动安装

1. 从 Releases 下载对应 ZIP，或从成功的 Actions 运行中下载构建产物。平台按 **CPA 所在主机或容器** 的系统和架构选择。
2. 解压，将动态库放入 CPA 插件目录对应的系统和架构子目录。以 Linux amd64 为例，路径为 `plugins/linux/amd64/mistral-bridge.so`。
3. 在 CPA 配置中启用插件并重启 CPA。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    mistral-bridge:
      enabled: true
```

| CPA 运行平台 | 支持架构 | ZIP 内的动态库 |
| --- | --- | --- |
| Linux | amd64、arm64 | `mistral-bridge.so` |
| macOS | amd64、arm64 | `mistral-bridge.dylib` |
| Windows | amd64、arm64 | `mistral-bridge.dll` |

Linux 发布包以 Ubuntu 22.04 为构建基线，适用于 glibc 2.35 及以上环境，包括 Debian Bookworm 的 CPA 镜像。Docker 部署时，插件目录需要挂载到容器内，并与 `plugins.dir` 一致。

### 第一次使用

1. 在 CPA / CPAMP 侧栏打开 **Mistral Bridge**。独立管理页路径为 `/v0/resource/plugins/mistral-bridge/console`。
2. 在 **凭证管理 → 添加凭证** 中填写 Mistral Key。默认 API 地址为 `https://api.mistral.ai/v1`。
3. 进入 **模型映射**，点击 **拉取目录**，再通过 **添加映射** 选择上游模型、填写请求模型名称，并设置为 **已启用**。
4. 等待“模型列表已同步到 CPA”提示，在客户端填写 CPA 的 API 地址和客户端 API Key，选择刚配置的请求模型。

首次安装的凭证、目录、映射与日志为空。拉取目录会更新上游候选列表，客户端可用模型由已启用的映射决定。独立打开管理页时，使用该面板的管理密钥连接。

## 调用模型

例如配置以下映射。

| 请求模型 | 上游模型 |
| --- | --- |
| `mistral-small` | `mistral-small-latest` |

客户端向 CPA 的 `/v1/chat/completions` 发送请求，`model` 填 `mistral-small`。

```json
{
  "model": "mistral-small",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

请求模型名直接用于 CPA 的模型列表和调用。凭证绑定留空时，优先选择已启用凭证中优先级最高的一组，并在组内轮询；指定凭证时使用该凭证。

思考强度映射可填写 `high=medium, low=none`。留空会传递请求中的强度，最终可用值取决于上游模型。

## 额度、日志与数据

| 项目 | 行为 |
| --- | --- |
| 调试明细 | 默认关闭。启用后保存完整上游请求和响应，可能包含提示词与模型输出；认证头及已配置 Key 会脱敏。仅保留最近 100 条且总量最多 256 MiB，超过单条上限的明细不会保存。关闭模式不会自动删除已有明细。 |
| 月度额度 | 根据 Key 类型读取 API 或 Vibe 预算，展示剩余量、用量比例、重置时间与刷新状态。同一组织的多个 Key 共享预算。 |
| 额度不可用 | 显示接口返回的状态或错误；存在历史结果时保留结果与时间。自定义 API 地址显示额度查询不可用。 |
| 请求明细 | 时间、请求模型、上游模型、思考强度、结果、掩码凭证、首字时间、总耗时与 Token 用量。 |
| 统计范围 | 对保留的最近 10,000 条日志按筛选条件统计，分页显示明细。历史缺失字段显示 `—`。 |
| 缓存命中率 | 已返回缓存用量的请求中，缓存 Token 总量占输入 Token 总量的比例。 |
| 平均 Token 速率 | 成功流式请求的输出 Token 总量，除以首字后的生成耗时总和。非流式请求的首字时间显示 `—`。 |

状态保存在 `<auth-dir>/plugin-data/mistral-bridge/state.json`，请求元数据保存在同目录的 `requests.jsonl`，调试明细位于 `debug-logs/`。未取得 CPA `auth-dir` 时使用 `$HOME/.cli-proxy-api/plugin-data/mistral-bridge`，也可通过 `MISTRAL_BRIDGE_DATA_DIR` 指定目录。

`state.json` 包含上游 Key，页面仅展示掩码。容器部署请持久化插件目录和数据目录。要重新体验首次使用流程，可以停止 CPA 后备份并移走该插件的数据目录，再启动 CPA。

## 从源码构建

使用 Go 1.26.11 和目标平台的 C 编译器。在 Linux amd64 环境中执行以下命令。

```sh
CGO_ENABLED=1 GOAMD64=v1 CGO_CFLAGS="-O2 -g -march=x86-64" \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags="-s -w -X main.pluginVersion=0.2.1" \
  -o build/mistral-bridge.so .
```

其他平台采用原生 runner 构建，CPU 基线为 amd64 v1 或 ARMv8.0。测试与打包命令在发布工作流中维护。[构建工作流](.github/workflows/build.yml)

Windows ARM64 使用 `windows-11-arm` runner 和 MSYS2 CLANGARM64 的 Clang、原生 Python，构建 DLL 后执行 C ABI 加载验证。Go 不支持 Windows ARM64 的 race 检测，因此该平台运行普通 Go 测试，其余平台保留 race 检测。

```sh
go test -race -count=1 ./...
go vet ./...
node --check web/app.js
python3 -m unittest discover -s scripts -p 'test_*.py' -v
```

## 自动构建与发布

推送到 `main` 会执行测试、检查、六个平台的构建及动态库加载验证，并生成保留 14 天的 `mistral-bridge-dev-<commit>` Actions 产物。

推送 `v0.2.1` 这样的数字版本 tag，会执行相同流程，并在全部通过后创建带发布说明的 **Release 草稿**。插件元数据使用 tag 中的版本号。草稿包含六个平台的 ZIP 和 SHA-256 `checksums.txt`，ZIP 名称采用 `mistral-bridge_<version>_<goos>_<goarch>.zip`，内部根目录放置对应动态库。

```sh
git tag v0.2.1
git push origin v0.2.1
```

请将示例版本替换为本次发布版本。重跑会更新已有草稿及附件；已公开的版本受到保护。审核草稿后手动发布，商店通过 GitHub 最新发布接口发现版本。工作流使用仓库自动提供的 `GITHUB_TOKEN`。[插件商店规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)

## 致谢

感谢 CPA 提供插件运行时、magpie 提供 Mistral 协议适配参考，以及 ClinePassBridge 的管理界面与文档组织参考。[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) · [magpie](https://github.com/yetone/magpie) · [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)

项目图标基于 Mistral 像素标记增加桥接元素，用于识别此社区插件。Mistral 名称与原始标记归 Mistral 所有。[品牌素材](https://mistral.ai/brand/)
