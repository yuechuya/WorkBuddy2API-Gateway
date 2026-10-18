# WorkBuddy2API-Gateway 使用手册

> 多协议 API 网关：把 **Anthropic Messages**、**OpenAI Responses**、**OpenAI Chat Completions** 三种协议统一接入，经过账号池治理（选号 / 轮转 / 熔断 / 冷却 / 粘性 / 降级 / 用量记账）转发到上游 CodeBuddy 账号，并附带内嵌的 Web 管理面板。
>
> 本手册面向**使用者与运维者**：怎么启动、怎么用、怎么配置。发布相关的操作见 [docs/publishing.md](publishing.md)。

## 目录

- [1. 这是一个什么系统](#1-这是一个什么系统)
- [2. 快速开始](#2-快速开始)
- [3. 启动与停止](#3-启动与停止)
- [4. Web 面板详解](#4-web-面板详解)
- [5. 客户端接入](#5-客户端接入)
- [6. 模型名与映射规则](#6-模型名与映射规则)
- [7. 配置完全参考](#7-配置完全参考)
- [8. 账号与自动任务](#8-账号与自动任务)
- [9. 日常运维与排错](#9-日常运维与排错)
- [10. 常见问题 FAQ](#10-常见问题-faq)
- [11. 附：源码构建与 Docker](#11-附源码构建与-docker)
- [12. 链接](#12-链接)

---

## 1. 这是一个什么系统

```
                ┌──────────────────────────┐
  Claude Code ──┤                          │
   Codex CLI ───┤  三种入站协议              │  账号池治理（选号/轮转/熔断/
       DSH ────┤  /v1/messages             │  冷却/粘性/降级/记账）
     ZCode ────┤  /v1/responses             ├──────────────► 上游 CodeBuddy
  OpenAI SDK ──┤  /v1/chat/completions      │               （按积分计费）
                └──────────────────────────┘
                          │
                    Web 管理面板 /panel/
```

三个要点：

1. **三种协议同一个入口**。外来协议（Anthropic / Responses）的请求会被转换成本机 chat/completions 形态，走同一条治理管线，响应再翻译回目标协议。协议差异只存在于「请求解析」和「响应编码」两段，不为第二、三套协议复制轮转逻辑。
2. **模型名自动映射**。Anthropic / Responses 客户端只会说自己的模型名（`claude-sonnet-4-5`、`gpt-5-codex`…），网关按可配置的别名表映射成上游真实模型名（见 [第 6 章](#6-模型名与映射规则)）。
3. **一切状态可在面板观察**。账号健康、用量积分、每次请求的协议与模型映射、自动任务执行，都在 `/panel/` 里。

环境要求：任意 64 位 Windows / Linux / macOS；默认监听 `:7863`；除二进制外无外部依赖（Redis 可选，用于多实例共享粘性）。

---

## 2. 快速开始

### 2.1 获取程序

三种方式，任选其一：

| 方式 | 操作 | 适合 |
|---|---|---|
| **Release 二进制（推荐）** | 从仓库 Releases 页下载对应平台的压缩包，解压即用 | 所有用户，无需 Go 环境 |
| **源码构建** | 见 [第 11 章](#11-附源码构建与-docker)，一条 `go build` 命令 | 需要改代码 |
| **Docker** | `docker compose up -d`（镜像走 GHCR） | 服务器常驻 |

### 2.2 目录结构

解压 /  clone 后的典型布局：

```
WorkBuddy2API-Gateway/
├─ bin/
│  └─ wb2api.exe          # 服务二进制（Release 包已含；源码构建产出）
├─ cmd/server/            # 服务端入口（源码构建用）
├─ internal/              # 全部源码（Go）
├─ config.json            # 【你的配置】首次启动自动生成，含随机 api_key
├─ config.example.json    # 配置模板（所有字段及默认值）
├─ auths/                 # 账号凭证目录（面板加号产生；明文，勿外传）
├─ data/                  # 池状态 + 请求日志归档（JSONL）
├─ start.bat / stop.bat   # Windows 启停脚本
├─ docs/                  # 文档（本手册、发布手册）
└─ Dockerfile / docker-compose.yml
```

> `config.json`、`auths/`、`data/`、`bin/` 是运行期产物，`.gitignore` 已全部排除——**升级时只换二进制，这三个原样保留**。

### 2.3 首次启动（Windows）

双击 `start.bat`。脚本会做四件事：

1. 检查 `wb2api.exe` 存在——Release 解压的 zip 里它在脚本同级根目录，源码树/本地构建时在 `bin\` 子目录，两种布局都认（都没有则提示去 Releases 下载或源码构建）；
2. 检查 7863 端口——已在监听就打印 `[SKIP]` 直接退出（幂等，不会端口冲突）；
3. 自动创建 `auths/` 和 `data/` 目录；
4. 启动服务，窗口里打印面板 / 健康检查地址。

首次启动若没有 `config.json`，会自动生成一份推荐配置，**随机 api_key 会打印在启动日志里**（形如 `config ... 已生成推荐配置（api_key=sk-...）`），抄下来，后面客户端和面板登录都用它。

启动成功的标志：

```
Panel  : http://127.0.0.1:7863/panel/
Health : http://127.0.0.1:7863/healthz
```

### 2.4 三十秒自测

任选一个终端（PowerShell / bash），确认三件事：

```powershell
# ① 健康检查（无需密钥）：返回 JSON，total 是账号数、healthy 是健康数
curl http://127.0.0.1:7863/healthz

# ② 面板能打开（浏览器）：根路径会自动跳转到 /panel/
start http://127.0.0.1:7863/

# ③ 发一发对话（把 <api_key> 换成 config.json 里的值）
curl -X POST http://127.0.0.1:7863/v1/chat/completions `
  -H "Authorization: Bearer <api_key>" `
  -H "content-type: application/json" `
  -d '{\"model\":\"glm-5.3-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'
```

③返回 `choices[0].message.content` 即全链路打通。此时账号池是空的（total=0）——去面板「账号池」视图点加号完成 OAuth 授权（见 [第 8 章](#8-账号与自动任务)），加完号再发一次即可。

### 2.5 开机自启（可选）

把 `start.bat` 的快捷方式丢进启动目录：

```
%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup
```

登录后自动拉起。注意 start.bat 会开一个窗口，**别关它**（关窗即停服，见 [第 3 章](#3-启动与停止)）。

---

## 3. 启动与停止

| 操作 | 方法 |
|---|---|
| 启动 | 双击 `start.bat`（幂等：已在运行会 `[SKIP]`） |
| 停止 | 双击 `stop.bat`，或直接关掉 start.bat 的窗口 |
| 重启 | 先 stop 再 start；或关窗后重新双击 |

**关于那个黑窗口**：服务是前台进程，窗口即服务。最小化可以，**关闭 = 停服**。这是设计使然（日志直接打在窗口里，方便观察），不是故障。

日志三个去处：

1. **start.bat 窗口**：实时滚动，请求日志是表格行（时间 / 模型 / 状态码 / 账号 / Token / 耗时）；
2. **面板「运行日志」视图**：结构化查询（见 [4.7](#47-运行日志)）；
3. **磁盘归档**：`data/request-logs/requests-*.jsonl`，只写请求元数据（模型、账号标签、Token、耗时、结果），**不写提示词、响应正文和密钥**；保留天数 / 大小上限在 config 的 `logging` 段配。

源码 / Docker 部署时没有窗口，日志走 stdout / `docker logs`。

---

## 4. Web 面板详解

浏览器打开 `http://127.0.0.1:7863/`（自动跳 `/panel/`）。若 `api_key` 非空，首次弹出密钥门——输入 `config.json` 里的 `api_key`（浏览器会记住）。

### 4.1 账号池（首页）

- 账号卡片：昵称、积分、健康状态（健康 / 冷却中 / 熔断中 / 已禁用）、在途数、粘性绑定；
- 顶部常驻**接入信息卡**：三种协议的端点（自动按当前访问地址推导）+ API Key 回显，端点一键复制——新用户第一个问题（“地址填什么”）第一眼有解；
- **加号**：添加账号，走 OAuth 授权流程（Windows 上等价于上游的 `login.sh`，由面板代劳）；
- 单账号操作：禁用 / 启用 / 删除。

### 4.2 用量

按时间范围（今天 / 近 24 小时 / 近 7 天 / 自定义…）统计请求数、成功率、Token 消耗、积分消耗，按账号 / 模型拆分。与「运行日志 → 请求记录」共用同一个时间范围控件。

### 4.3 积分构成

逐账号实时查上游的积分明细（余额 / 成长值 / 各任务奖励）。这是直连上游的实时查询，点一次打一次，按需使用。

### 4.4 模型与档位

上游模型目录 + 每模型的能力档位（上下文长度、最大输出、思考档位、倍率）。**给客户端配模型 ID 时，来这里复制精确全名**（见 [第 6 章](#6-模型名与映射规则)）。

### 4.5 配置

config.json 的图形化表单。保存行为：

- **原子落盘**：先写临时文件再 rename，不会写坏配置；
- **保留未知键**：只覆盖表单管理的键，你手写加进 config.json 的其他字段不会被洗掉；
- **热生效 + 重启提示**：能立即生效的当场应用，仍需重启的字段在保存结果里列出来（完整的热 / 重启边界见 [7.1](#71-两种改法与热生效边界)）。

### 4.6 任务中心

六类自动任务（签到 / 成长 / 旅行 / 活动 / 保活 / 黑猫）的执行状态与历史，余额刷新周期也在这里观察。任务时间点在「配置」视图改，热生效。

### 4.7 运行日志

分上下两块：

- **请求记录表**：时间 / 结果 / **协议** / 模型 / 账号 / 来源 IP / User-Agent / 耗时 / Token / 积分 / 请求 ID。
  - **协议列**（OpenAI / Anthropic / Responses 三色徽标）体现多协议入站——三种协议共用同一条治理管线，记账按真实端点区分；
  - 发生模型名映射时，模型列显示「**原始名 → 解析名**」（如 `claude-sonnet-4-5 → glm-5.2`）；
  - 筛选：结果下拉 + 搜索框（IP / UA / 模型，**含原始名** / 账号 / 协议 / 请求 ID，空格分词 AND）+ 时间范围 + 读取条数（100 / 300 / 1000）；
  - 行悬停有完整 tooltip；来源列需要 `logging.request_client_info` 开启（默认开）。
- **运行日志流**：按「任务 / 对话 / 系统」三个频道过滤的实时日志，可开关自动滚动。对话流量再大，任务结果也不会被冲掉。

---

## 5. 客户端接入

### 5.0 通用规则（先读这一节）

**Base URL 形态**——两种，别混：

| 客户端 | Base URL 填 | 原因 |
|---|---|---|
| Claude Code | `http://127.0.0.1:7863` | 客户端自己会拼 `/v1/messages` |
| 其余全部 | `http://127.0.0.1:7863/v1` | 客户端拼 `/chat/completions`、`/responses` |

**鉴权**：一律 `Authorization: Bearer <api_key>`（`api_key` 留空 = 不鉴权，仅建议本机测试用）。注意网关**只认 Bearer 头，不认 `x-api-key`**——所以 Claude Code 必须用 `ANTHROPIC_AUTH_TOKEN` 环境变量（它发 Bearer），用 `ANTHROPIC_API_KEY` 会 401。

**模型 ID**：用面板「模型与档位」视图或 `GET /v1/models` 返回里的**精确全名**（带 `cn:` / `global:` 前缀）。别名映射只对 Anthropic / Responses 协议生效，chat/completions 是原样透传——细节见 [第 6 章](#6-模型名与映射规则)。

### 5.1 Claude Code（Anthropic 协议）

```powershell
$env:ANTHROPIC_BASE_URL  = "http://127.0.0.1:7863"   # 不带 /v1
$env:ANTHROPIC_AUTH_TOKEN = "<api_key>"               # 不是 ANTHROPIC_API_KEY
claude
```

模型名随便说：`claude-sonnet-4-5`、`claude-opus-4-1` 都会按别名表映射（默认 sonnet→glm-5.2、opus→deepseek-v4-pro）。想固定就在 `/model` 里选，或直接说真实模型名。

`count_tokens` 端点（Claude Code 的上下文预警会调）网关本地估算，不影响使用。

### 5.2 Codex CLI（Responses 协议）

`~/.codex/config.toml`：

```toml
model = "gpt-5.3-codex"
model_provider = "wb2api"

[model_providers.wb2api]
name = "workbuddy2api-gateway"
base_url = "http://127.0.0.1:7863/v1"
wire_api = "responses"
```

配合环境变量 `OPENAI_API_KEY=<api_key>`（Codex 发的是 Bearer，没问题）。

`model` 含 `codex` 会按别名表映射到 `kimi-k2.7`（代码特化）；想用别的模型直接写目录全名（如 `cn:glm-5.2`）。注意 Codex 0.144+ 已移除 chat 模式，`wire_api` 必须是 `"responses"`。

### 5.3 DSH

自定义提供方表单：

| 字段 | 值 |
|---|---|
| API 地址 | `http://127.0.0.1:7863/v1`（**必须带 `/v1`**，不带探活过、请求 404） |
| 协议 | `openai-responses` |
| 密钥 | `<api_key>` |
| 模型 | 目录全名，如 `cn:glm-5.2` |

### 5.4 ZCode

设置 → 模型供应商 → 添加模型供应商 → **自定义端点**：

| 字段 | 值 |
|---|---|
| 名称 | 随意（如 WorkBuddy） |
| Base URL | `http://127.0.0.1:7863/v1` |
| API 格式 | Chat Completions（或 Responses / Anthropic Messages） |
| API Key | `<api_key>` |
| 模型 ID | 目录**精确全名**，如 `cn:glm-5.2`、`cn:deepseek-v4-flash`、`cn:kimi-k2.7` |

⚠️ **ZCode 的模型 ID 必须与目录精确匹配**。填裸名（`glm-5.2`）或显示名（`DeepSeek-V4-Flash`）会报 `11102 service info not found`——ZCode 在目录里查不到对应服务信息。改完模型 ID 要**完全退出 ZCode 再重开**才生效。

### 5.5 OpenAI SDK 及其他（Cherry Studio / LangChain / curl…）

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:7863/v1", api_key="<api_key>")
resp = client.chat.completions.create(
    model="cn:glm-5.2",                     # 目录全名
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

流式：`stream=True`，SSE 逐帧返回。请求体上限 8 MiB。

---

## 6. 模型名与映射规则

### 6.1 上游目录长什么样

`GET /v1/models`（或面板「模型与档位」视图）返回上游真实目录，53 个模型，全部带域前缀：

```
cn:glm-5.3-flash    cn:glm-5.2    cn:glm-5.3    cn:deepseek-v4-flash
cn:deepseek-v4.1-flash  cn:deepseek-v4-pro  cn:kimi-k2.7  cn:kimi-k2.8-preview
cn:kimi-k3-1  cn:hy3  cn:hy4-preview  cn:step-5-preview  cn:minimax-m3 …
global:gpt-5.3-codex  global:gpt-6-sol  global:gemini-3.5-flash  global:grok-4.7 …
```

（清单随账号变动，以实时返回为准。）

### 6.2 三种协议下的模型名行为

| 协议 | 行为 |
|---|---|
| `/v1/chat/completions` | **原样透传，不做映射**。目录全名（`cn:glm-5.2`）和裸名（`glm-5.2`）都接受；目录外的名字会被上游拒（`11102 model_unavailable`），且该错误名会触发模型级冷却（约 10 分钟内同名字请求报「账号暂不可用」） |
| `/v1/messages`、`/v1/responses` | **别名映射**：① 别名规则（通配，首中即停）→ ② 目录精确命中则透传 → ③ 都不中回落 `default_model` |

### 6.3 内置别名表（config 未配置 inbound 段时的默认）

| 外来模型名 | 解析到 | 定位 |
|---|---|---|
| `*haiku*` | `glm-5.3-flash` | 快而廉（最低倍率） |
| `*codex*` | `kimi-k2.7` | 代码特化 |
| `*sonnet*` | `glm-5.2` | 主力均衡 |
| `*opus*` | `deepseek-v4-pro` | 旗舰 + 思维链 |
| `*gpt-*` | `glm-5.2` | 其余 GPT 类 |
| 都不命中 | `glm-5.3-flash` | default_model 兜底 |

⚠️ **别名规则优先于目录透传**：`gpt-5.3-codex` 本身是目录里的真实模型，但因为它含 `codex`，会先被 `*codex*` 规则劫持到 `kimi-k2.7`。想用真实的 `global:gpt-5.3-codex`，就把 `*codex*` 规则删掉或改成更精确的 pattern。

### 6.4 自定义映射

config.json 加 `inbound` 段（改完**重启生效**，详见 [7.3](#73-inboundmodel_alias-专题二开核心)）：

```json
"inbound": {
  "model_alias": {
    "default_model": "glm-5.3-flash",
    "rules": [
      { "pattern": "*opus*",   "model": "deepseek-v4-pro" },
      { "pattern": "*sonnet*", "model": "glm-5.2" },
      { "pattern": "my-*",     "model": "cn:hy3" }
    ]
  }
}
```

规则语义：`pattern` 支持 `*`（任意串）/ `?`（单字符），大小写不敏感，**按顺序首中即停**；你的 rules 在前、内置表兜底——只加一条不会弄丢其他映射；`default_model` 都不命中时兜底。

每次映射会在运行日志记一行（同组合只记一次）；效果去面板「运行日志」页看「原始名 → 解析名」。

---

## 7. 配置完全参考

配置文件：`config.json`（模板 `config.example.json`）。所有时长字段是 Go duration 字符串（`"600s"`、`"30m"`、`"2h"`）。

### 7.1 两种改法与热生效边界

| 改法 | 适用 |
|---|---|
| **面板「配置」视图** | 热生效项，保存即用，自动提示仍需重启的字段 |
| **手改 config.json + 重启** | 重启项；以及面板未暴露的段（如 inbound） |

**热生效（保存即用）**：`api_key`、`pool.*`、`cooldown.*`、`schedule.*`、`features.*`、`logging.request_client_info`。

**需重启**：`listen`、`auth_dir`、`state_file`、`upstream.*`（含 `user_agent`）、`upstash`、`session_sticky.*`、`logging.request_archive_enabled/retention_days/archive_max_mb`、`server.read_timeout`、`inbound.*`。

> 规则：未列入热生效清单的段，改完一律重启才生效。`prompt.*`、`global.*`、`panel.*` 均属此类。

### 7.2 逐段参考

#### 基础

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | 监听地址；改 IP/端口需重启。冲突时改这里 |
| `api_key` | 首次启动随机生成 | 客户端与面板的 Bearer 密钥；**留空 = 不鉴权** |
| `auth_dir` | `./auths` | 账号凭证目录；需重启 |
| `state_file` | `./data/state.json` | 池状态文件；需重启 |

#### `server`

| 字段 | 默认 | 说明 |
|---|---|---|
| `read_timeout` | `300s` | 请求读取超时；需重启 |

#### `panel`

| 字段 | 默认 | 说明 |
|---|---|---|
| `package_detail_limit` | `5` | 积分构成视图每账号展示的明细条数 |

#### `logging`（请求归档）

| 字段 | 默认 | 说明 |
|---|---|---|
| `request_archive_enabled` | `true` | 是否写 JSONL 归档（`data/request-logs/`）；**需重启** |
| `request_retention_days` | `7` | 归档保留天数；需重启 |
| `request_archive_max_mb` | `100` | 归档总大小上限；需重启 |
| `request_client_info` | `true` | 是否记录来源 IP / UA（面板日志列与筛选）；**热生效** |

归档只写请求元数据（模型、账号标签、Token、耗时、结果、来源），不写提示词、响应正文、Authorization。

#### `cooldown`（软限流退避）

| 字段 | 默认 | 说明 |
|---|---|---|
| `soft_rate` | `600s` | 软限流基础冷却；连续触发按指数升级 |
| `soft_rate_max` | `2h` | 指数升级上限；热生效 |

#### `pool`（账号池治理）

| 字段 | 默认 | 说明 |
|---|---|---|
| `max_in_flight` | `3` | 单账号并发在途上限 |
| `max_in_flight_global` | `2` | 全局每域在途上限 |
| `breaker_threshold` | `3` | 连续失败这么多次进熔断 |
| `breaker_cooldown` / `breaker_cooldown_max` | `30m` / `6h` | 熔断冷却与上限 |
| `degrade_threshold` / `degrade_cooldown` / `degrade_cooldown_max` | `5` / `10m` / `2h` | 连败降权阈值与冷却 |
| `idle_weight_per_hour` / `idle_weight_max` | `0.5` / `5.0` | 闲置补偿选号权重 |
| `prefer_expiring` | `true` | 优先选临期账号 |
| `expiring_soon` | `168h` | 「临期」窗口（7 天） |
| `cost_explore_interval` | `30m` | 成本档位探索窗口（0 = 关闭） |
| `credit_floor` | `100` | 积分保底：低于此值的账号不参选（0 = 关闭） |

全部热生效。

#### `schedule`（自动任务）

| 字段 | 默认 | 说明 |
|---|---|---|
| `checkin_hours` | `[9, 21]` | 签到时间点（24h 制，可多选） |
| `growth_hours` | `[1]` | 成长值任务 |
| `travel_hours` | `[9, 21]` | 旅行任务 |
| `activity_hours` | `[10]` | 活动任务 |
| `keepalive_hours` | `[22]` | 保活任务 |
| `blackcat_hours` | `[23]` | 黑猫任务 |
| `*_enabled` | 全 `true` | 各任务开关 |
| `balance_refresh_enabled` / `balance_refresh_minutes` | `true` / `5` | 余额后台刷新 |
| `include_disabled_in_tasks` | `false` | 禁用账号是否也跑任务 |

全部热生效。

#### `global`（双域路由）

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `true` | 是否启用 global 域账号（cn 域恒可用） |
| `chat_base` / `billing_base` | 空 | 上游地址覆盖（一般留空走默认） |

#### `upstream`

| 字段 | 默认 | 说明 |
|---|---|---|
| `timeout_seconds` | `120` | 上游总超时 |
| `header_timeout_seconds` | `120` | 响应头超时 |
| `idle_timeout_seconds` | `300` | 流式空闲超时 |
| `user_agent` / `client_version` / `cli_version` / `client_name` | 空 | 出站客户端标识覆盖（留空用内置） |
| `device_token` / `device_token_file` | 空 | 设备令牌（一般不用管） |
| `passthrough_ip` | `false` | 是否把客户端 IP 透传上游 |

全部需重启。

#### `session_sticky`（会话粘性）

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `true` | 同一会话优先绑同一账号 |
| `ttl` | `30m` | 粘性存活时长 |
| `gc_interval` | `5m` | 清理周期 |

需重启。多实例共享粘性可配 Redis（见 `upstash`）。

#### `features`

| 字段 | 默认 | 说明 |
|---|---|---|
| `sanitize_blacklist_fingerprints` | `true` | 出站脱敏（清黑名单指纹）；热生效 |

#### `prompt`

| 字段 | 默认 | 说明 |
|---|---|---|
| `mode` | `passthrough` | `passthrough` = 提示词原样转发；`custom` = 用网关自有提示词 |
| `file` | 空 | custom 模式的提示词文件路径 |

#### `upstash`（可选 Redis）

| 字段 | 默认 | 说明 |
|---|---|---|
| `url` / `token` | 空 | 配了即用 Redis 共享粘性 / 状态；留空用本地 |

### 7.3 `inbound.model_alias` 专题（二开核心）

本仓库相对上游的新增能力：外来协议（Anthropic / Responses）的模型名映射。结构：

```json
"inbound": {
  "model_alias": {
    "default_model": "glm-5.3-flash",
    "rules": [
      { "pattern": "*haiku*", "model": "glm-5.3-flash" },
      { "pattern": "*codex*",  "model": "kimi-k2.7" },
      { "pattern": "*sonnet*", "model": "glm-5.2" },
      { "pattern": "*opus*",   "model": "deepseek-v4-pro" },
      { "pattern": "*gpt-*",   "model": "glm-5.2" }
    ]
  }
}
```

- 整段省略 = 用内置默认表（等价于上面这份）；
- `pattern` 通配、大小写不敏感、首中即停；
- 你的 rules 在前 + 内置兜底；
- `default_model` 留空则别名与目录都不命中时**原样透传**（让上游给真实错误）；
- **改完必须重启**（面板暂无编辑器，手改 JSON）；
- 目标模型名以目录实时返回为准——上游清单会变，映射表跟着账号走，[6.3](#63-内置别名表config-未配置-inbound-段时的默认) 的表经真实账号目录核对，不合适就改。

---

## 8. 账号与自动任务

### 8.1 加号与账号状态

面板「账号池」→ 加号 → OAuth 授权。授权后凭证落在 `auths/*.json`（**明文 accessToken/refreshToken，勿外传**；`.gitignore` 已排除）。

账号状态机：健康 →（软限流）冷却 →（连续失败）熔断 →（手动）禁用。`/status` 端点与面板同口径展示，含每账号在途 / 粘性 / 模型级锁定（`model_locks`：按「域 + 模型」聚合的限流视图）。

### 8.2 自动任务

六类任务替账号做上游日常（签到 / 成长 / 旅行 / 活动 / 保活 / 黑猫），时间点在 `schedule` 段或面板「配置」改，热生效；执行历史在「任务中心」。余额每 5 分钟后台刷新（`balance_refresh_minutes`）。

**跑任务的窗口不能关**——任务由服务进程自己调度，关窗即停排程。

---

## 9. 日常运维与排错

### 9.1 错误码速查

| 现象 | 原因 | 处理 |
|---|---|---|
| `401 invalid_api_key` | 没带 / 带错密钥 | 检查 `Authorization: Bearer <api_key>`；`api_key` 留空时不鉴权 |
| `503` / `no_healthy_account` / 「账号暂不可用」 | 池里没有可选之号：全在冷却/熔断、在途占满，或**模型级冷却** | `curl /status` 看池子；等冷却（软限流约 10 分钟起）；加号扩池 |
| `11102 model_unavailable` | 模型名不在目录（chat 协议不映射） | 换目录精确全名 |
| ZCode `11102 service info not found` | 模型 ID 与目录不精确匹配 | 换目录全名 + 重启 ZCode |
| 面板打不开 / 客户端连不上 | 服务没起 / 端口不符 | `netstat -ano \| findstr :7863`；确认 Base URL 形态（[5.0](#50-通用规则先读这一节)） |
| Claude Code 401 | 用了 `ANTHROPIC_API_KEY` | 改用 `ANTHROPIC_AUTH_TOKEN`（Bearer） |
| 思考型模型（kimi 系）回复为空 | `max_tokens` 太小，思考就占满了 | 调大 max_tokens |

### 9.2 升级

1. 停服（`stop.bat` 或关窗）；
2. 用新二进制替换 `bin/wb2api.exe`（或重新解压 Release 包，**保留 `config.json` / `auths` / `data`**）；
3. 重新 `start.bat`。

配置、账号、归档全部向前兼容；新版若新增配置段，省略即走默认。

### 9.3 备份清单

定期复制这三样即可完整迁移：

```
config.json      # 配置与密钥
auths/           # 账号凭证
data/            # 池状态与请求归档
```

### 9.4 端口被占

`bind: Only one usage of each socket address` → 改 `listen`（如 `:7864`）→ 重启；或先停掉占用 7863 的旧实例。start.bat 自带占位检测，已在运行会 `[SKIP]`。

---

## 10. 常见问题 FAQ

**Q：api_key 忘了？**
看 `config.json` 的 `api_key` 字段；或首次启动日志里打印的那把。

**Q：想让局域网 / 团队用？**
`listen` 改 `0.0.0.0:7863` 并设置强 `api_key`（留空 = 任何人可打）。注意 `auths/` 是明文凭证，部署机器别裸奔。

**Q：三种协议能同时用吗？**
能。三种协议共用同一条治理管线，账号池、粘性、记账完全一致；面板日志按真实协议区分。

**Q：CCR / claude-code-router 还需要吗？**
不需要。本仓库的 Anthropic / Responses 入站就是原生替代——客户端直连 7863，少一跳。

**Q：模型名映射能可视化调吗？**
目前别名表手改 `config.json` 的 `inbound` 段 + 重启；映射效果在面板「运行日志」页实时可见（原始名 → 解析名）。

**Q：请求记录存什么？会泄密吗？**
只存元数据（时间 / 模型 / 账号标签 / Token / 耗时 / 结果 / 可选来源 IP·UA），不存提示词与响应正文。`logging.request_client_info` 关掉后来源也不落盘。

**Q：Docker 部署？**
`docker compose up -d`，镜像在 GHCR；挂载 `config.json` / `auths` / `data` 三个卷。详见 [publishing.md](publishing.md) 与 Dockerfile 注释。

**Q：怎么确认跑的是新版？**
`/healthz` 响应带 `service` 字段；面板有「接入信息」卡和日志页「协议」列即为本仓库版本。

---

## 11. 附：源码构建与 Docker

### 源码构建

要求 Go 1.22.5+：

```bash
go build -trimpath -ldflags="-s -w" -o bin/wb2api.exe ./cmd/server
```

（Windows 产物 `wb2api.exe`；Linux/macOS 去掉 `.exe`。其余子命令 `cmd/login`、`cmd/signin`、`cmd/credit`、`cmd/trial` 按需构建。）

测试：

```bash
go test ./...
```

### Docker

```bash
docker compose up -d        # 或 docker build -t workbuddy2api-gateway .
```

容器内监听 7863；把 `config.json`、`auths/`、`data/` 挂载进容器。

---

## 12. 链接

**上游（致谢，MIT 链）**：

- 原版项目：<https://github.com/Sliverkiss/workbuddy2api>（原仓库已被删除）
- panel 分支：<https://github.com/linguo2625469/workbuddy2api-panel>

**本仓库文档**：

- 本手册：`docs/user-manual.md`
- 发布到 GitHub 的操作清单：`docs/publishing.md`

**许可**：MIT。再分发时请保留 LICENSE 中的三级版权声明。本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利，使用者需自行遵守上游服务条款。
