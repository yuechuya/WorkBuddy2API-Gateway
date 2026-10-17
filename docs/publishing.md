# 发布到 GitHub 的操作手册

本文件是**发布当时**的对照清单。正常开发不需要看它；只有准备 `git push` / 打 Release 时才用得上。

---

## 0. 发布前终检（每次 push 前跑一遍）

```bash
cd <仓库根目录>

# 1) 敏感信息扫描（应全部无输出）
grep -rIl -E "haocheng|ZoyProjects|C:/Users/|D:/wb|zoy-digitalization" . | grep -v "^./.git"
grep -rIl -E "sk-[a-zA-Z0-9]{16,}|Bearer [a-zA-Z0-9._-]{20,}" . | head

# 2) 确认运行期产物不在版本控制内（应只有 .gitignore 命中、无实际文件）
ls config.json auths data logs bin 2>/dev/null

# 3) 构建 + 全量测试
go build ./... && go test ./...
```

任何一项有输出，先处理再 push。

## 1. 模块路径替换（必须，只做一次）

`go.mod` 当前是占位模块路径，发布前按实际 GitHub 用户名全局替换：

```bash
# 把 yourname 换成你的 GitHub 用户名（仓库名保持不变）
grep -rl "github.com/yuechuya/WorkBuddy2API-Gateway" . --include="*.go" --include="*.md" --include="*.mod" \
  | xargs sed -i 's|github.com/yuechuya/WorkBuddy2API-Gateway|github.com/<你的用户名>/wb2api-gateway|g'
```

> 替换后 `go build ./... && go test ./...` 要重跑一遍确认无残留引用。

文档中的占位符同步检查：

- `README.md`：GHCR 镜像地址 `ghcr.io/yourname/wb2api-gateway`、clone 地址
- `docs/user-manual.md`：clone 地址

## 2. 仓库初始化与首次推送

```bash
git init -b main
git add .
git commit -m "feat: multi-protocol inbound gateway (anthropic/responses/chat) + web panel"

# 先建空仓库再推（GitHub 网页建仓库时不要勾选 Add README，避免冲突）
git remote add origin https://github.com/<你的用户名>/WorkBuddy2API-Gateway.git
git push -u origin main
```

推送后立刻做两件事：

1. **检查 GitHub 仓库页面搜索 `sk-`、`Bearer`、`token`**——肉眼过一遍 README 和 config.example.json，确认没有真实值。
2. **Settings → Actions → General**：确认 Workflow 权限按需（本仓库 workflow 只在打 tag 时构建二进制，不需要写权限）。

## 3. GHCR 镜像（可选）

`.github/workflows/` 会构建 Docker 镜像并推 GHCR。镜像包默认 **private**，需要手动改公开：

- 进 `https://github.com/users/<你的用户名>/packages/9890456`（仓库名即包名）
- Package settings → Change visibility → **Public**

不改的话别人 `docker pull ghcr.io/...` 会 401。

## 4. 打 Release（二进制走 Release，不进仓库）

```bash
git tag v2.0.0
git push origin v2.0.0
```

workflow 会自动构建多平台二进制并挂到 Release 页。用户从 Release 下载，不需要自己编译。

版本号规则：workflow 会剥离 `-panel` / `-gateway` 后缀（`appVersion = "2.0.0-gateway"` → 产物 `v2.0.0`），保持 tag 干净。

## 5. 风险与合规要点

| 项 | 说明 |
|---|---|
| **上游家谱** | 本项目是 fork 的 fork：Sliverkiss/workbuddy2api（原版已删库）→ linguo2625469/workbuddy2api-panel → 本仓库。MIT 允许，但 LICENSE 与 README 已写明三级版权链，**不要删** |
| **DMCA / 封号风险** | 原版 Sliverkiss/workbuddy2api 曾被删库。fork 发布本身合规，但不要在 README/描述里诱导「薅羊毛」「无限积分」等表述；本仓库定位是「协议网关 + 管理面板」，介绍里避免任何攻击上游服务的措辞 |
| **上游服务条款** | 本项目不授予任何 CodeBuddy / 腾讯接口的权利，README 已声明；使用者行为与本仓库作者无关 |
| **issues 区** | 建议开 Issues；不要把任何账号凭证、config.json 内容贴进 issue |

## 6. 发布后维护建议

- 上游（linguo2625469 的 panel 分支）如果更新，合并时注意本仓库改过的文件：`internal/server/handler.go`（chatCore 拆分）、`internal/server/modelalias.go`、`internal/server/anthropic.go`、`internal/server/responses.go`、`internal/panel/index.html`、`internal/panel/app.js`、`cmd/server/config.go`、`cmd/server/main.go`
- 每次合并上游后重跑：`go build ./... && go test ./...`，重点看 `internal/server` 与 `internal/panel` 两个包的测试
