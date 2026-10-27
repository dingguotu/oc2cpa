# oc2cpa — CLIProxyAPI 的 OpenCode 免费模型插件

[English](README.md) | 简体中文

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 外置插件
（c-shared 动态库），把 **OpenCode Zen 免费模型** 变成 CLIProxyAPI 的一等
provider。

插件直接运行在 CLIProxyAPI 宿主进程内并直连上游——没有独立代理进程，也
没有多一跳 HTTP。本项目是原 Node.js 服务
[zhuweiyou/oc2api](https://github.com/zhuweiyou/oc2api) 的纯 Go 移植，在此
更名为 **oc2cpa** 并维护。

> 📌 OpenCode 在持续上新免费模型。`deepseek-v4.1-flash-free` 和
> `glm-5.3-flash-free` 是最新的两个，均为 200,000 上下文、支持图片输入。
> 模型对比见[免费模型](#免费模型)。

## 插件做什么

- **双端 OpenAI chat-completions。** 执行器声明了 `chat-completions`
  输入/输出格式，宿主会自动把 Claude/Gemini 协议的客户端翻译成
  chat-completions 再交给本插件。
- **保留门禁工具。** 免费层要求必须带一个 `read` 工具。插件负责注入它，把
  客户端工具改名成 `oc2cpa_*` 别名（重名时生成 `_1`、`_2` 后缀），并在响应
  侧把名字还原回来——包括跨流分片拆开的工具名。
- **上游只有流式。** 上游永远以 SSE 返回。客户端要非流式时，聚合器把整个
  流重组成一个 JSON 响应；流式请求则逐帧透传，期间完成工具调用分片合并与
  流索引规整。
- **会话亲和。** 每个凭据 holder 一个合成 `ses_…` 会话，30 分钟内复用，随
  全部 `x-opencode-session` 系列请求头发送。
- **真实错误语义。** 只有上游真的限流才是 `429`（`rate_limit_error`）；参数
  错误保持 `400`（`invalid_request_error`），鉴权失败保持 `401`/`403`
  （`authentication_error`），插件侧流异常使用 `502`
  （`upstream_stream_error`）。错误不会被一律伪装成额度不足。
- **瞬时失败重试。** 免费档上游会间歇性返回裸 `5xx` 或传输错误；插件在未出
  内容前对这些失败先做有限重试（共 3 次尝试），避免一次抖动就把宿主侧唯一
  凭据送进冷却。
- **看门狗超时。** 首个事件 30 秒、正文空闲 120 秒，`[DONE]` 之后还有 1 秒
  尾窗用来接住迟到的 `usage` 帧。建连超时只约束拨号与响应头——上游开始
  出流之后，生成过程永远不会被中途掐断。
- **合成凭据。** 一个 `oc2cpa.json` 凭据文件（首次注册时自动创建）即可使用；
  需要独立出口 IP 时可在里面填写 `proxy_url`；未填则回退使用宿主的全局
  `proxy-url`。

## 安装

### 插件商店（推荐）

```yaml
plugins:
  enabled: true
  store-sources:
    - https://github.com/dingguotu/oc2cpa/releases/latest/download/registry.json
```

然后在 CLIProxyAPI 管理界面安装 `oc2cpa`。

### 手动

```bash
# 以 linux/amd64 为例
unzip oc2cpa_<版本>_linux_amd64.zip     # zip 根目录就是 oc2cpa.so
cp oc2cpa.so /path/to/cliproxyapi/plugins/
```

也支持平台目录布局：`plugins/linux/amd64/oc2cpa.so`。插件 id 即动态库文件
名（去掉扩展名，`oc2cpa`）。

### 配置

```yaml
plugins:
  enabled: true
  configs:
    oc2cpa:
      enabled: true
      priority: 100
      upstream_base_url: https://opencode.ai/inference/v1
      connect_timeout_ms: 60000
      dynamic_models: false
```

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `upstream_base_url` | `https://opencode.ai/inference/v1` | 上游地址，可指向本地中继 |
| `connect_timeout_ms` | `60000` | 收到响应头前的等待毫秒数（1–600000） |
| `dynamic_models` | `false` | 除内置目录外，再从上游 `/models` 发现 `-free` 模型 |
| `usage_log` | `true` | 每次请求在宿主日志输出一行 `[oc2cpa-usage]` 转发日志（模型、延迟、token 消耗） |

无需 API Key——插件使用免费层要求的固定上游令牌鉴权。

## 免费模型

白名单放行 `big-pickle` 及所有以 `-free` 结尾的模型。常用且已验证的几个：

| 模型 | 上下文 | 输出 | 图片输入 |
| --- | --- | --- | --- |
| `big-pickle` | 200,000 | 32,000 | ❌ |
| `mimo-v2.6-flash-free` | 200,000 | 32,000 | ✅ |
| `deepseek-v4.1-flash-free` | 200,000 | 32,000 | ✅ |
| `glm-5.3-flash-free` | 200,000 | 32,000 | ✅ |

数据来源于
[https://models.opencode.ai/api.json](https://models.opencode.ai/api.json)
（`limit` 与 `modalities` 字段），新模型可自行核实，或设置
`dynamic_models: true` 自动拉取。

## 转发日志与用量

插件注册了 CLIProxyAPI 的 UsagePlugin 能力。宿主每次请求后会把用量记录推给
插件，插件向宿主日志输出一行 `[oc2cpa-usage]`：

```
[oc2cpa-usage] {"time":"2026-10-20T08:36:14+08:00","request_id":"...","provider":"oc2cpa","model":"glm-5.3-flash-free","auth_type":"oauth","api_key":"sk-mE279...","stream":true,"failed":false,"latency_ms":1318,"ttft_ms":1318,"input_tokens":247,"output_tokens":3,"total_tokens":250}
```

包含模型、流式/失败状态、总延迟与首字延迟（TTFT），以及
`input / output / reasoning / cached / total` 五项 token 计数（客户端 API
Key 已打码为前缀）；失败请求会带上游 `status` 码。同样的指标宿主自己也已
写入用量数据库，管理界面的统计页可以看到。`usage_log: false` 可关闭控制台
输出。

## 用量面板

插件同时通过 CLIProxyAPI 管理接口暴露了一个小型 Web 仪表盘：

- 面板页面：`/v0/resource/plugins/oc2cpa/panel`（时间范围 / 模型 / 状态筛选栏、
  汇总卡片、按模型 token 统计、最近请求列表，每 10 秒自动刷新）。
- 数据接口：`GET /v0/management/plugins/oc2cpa/usage/summary` 与
  `GET /v0/management/plugins/oc2cpa/usage/records?limit=100`
  （需要管理密钥；页面首次会要求输入一次，保存在浏览器本地）。两个接口
  都支持同一组筛选参数：

  | 参数 | 取值 | 默认 |
  |---|---|---|
  | `range` | `today` / `7d` / `30d` / `all` | `all` |
  | `model` | 精确模型 ID 或别名 | 全部模型 |
  | `status` | `ok` / `failed` / `all` | `all` |
  | `limit`（仅 records） | 返回最新命中的条数 | `100` |

面板统计来自插件进程内的记录窗口（宿主启动以来最近 5000 条请求）；持久
汇总仍在 CLIProxyAPI 自己的用量数据库里。管理前端支持插件资源时，该资源
会以 “OC2CPA” 菜单项（OpenCode 图标）出现在管理界面中。

## 开发

需要 Go 1.26+。构建本机动态库：

```bash
go build -buildmode=c-shared -o dist/oc2cpa.dll .        # Windows
# go build -buildmode=c-shared -o dist/oc2cpa.so .        # linux
# go build -buildmode=c-shared -o dist/oc2cpa.dylib .     # darwin
```

离线测试：

```bash
go test ./...
go vet ./...
```

跨平台构建与发布由
[`.github/workflows/build.yml`](.github/workflows/build.yml) 自动完成：推送
`v*` 标签会产出 `_linux_amd64`、`_linux_arm64`、`_darwin_amd64`、
`_darwin_arm64`、`_windows_amd64`、`_windows_arm64` 六平台压缩包，外加
`checksums.txt` 与 `registry.json`。

## 历史与致谢

oc2cpa 基于 [zhuweiyou/oc2api](https://github.com/zhuweiyou/oc2api) 修改——
原项目是独立 Node.js 代理，这里将其重写为纯 Go 的 CLIProxyAPI c-shared 插
件、更名并在本仓库维护。原协议工作与免费层逆向的成果完全归功于
[zhuweiyou](https://github.com/zhuweiyou)。许可为 MIT，见
[LICENSE](LICENSE)。
