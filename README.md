# VibeDock MVP 0.5

VibeDock 是一个单机、自托管的 AI 开发环境控制面。用户打开网页后，可以快速创建隔离的 Docker Workspace，导入 Git、ZIP 或本地文件，并在同一个 `8787` 工作台中使用 VS Code Web、多个 Agent Session、项目预览端口和持久化开发环境。

```text
Workspace
├─ persistent project volume
├─ persistent home volume
├─ VS Code Web
├─ Session: Codex ───── managed account / native login
├─ Session: Claude ──── managed account / native login
├─ Session: Pi Agent ── per-Session provider parameters
├─ Session: OpenCode ── per-Session provider parameters
├─ Session: Shell
└─ Preview ports
```

Workspace 是长期开发环境；Session 是一次独立的对话或工作任务。每个 Session 拥有自己的 tmux、Web Terminal、Agent 类型和凭据配置。关闭网页不会结束 Agent，同一个 Workspace 可以并行运行多个 Session。


## 0.5：Workspace 管理与内嵌工作台

0.5 将首页调整为操作密度更高的 Workspace 管理界面，并把运行工具作为工作台的一等对象。信息层级参考成熟的 Workspace 控制面：状态、资源、Session、更新时间与操作优先于装饰性内容；VibeDock 不复制第三方品牌、图标或文案。

工作台能力：

- 进入 Workspace 后直接恢复上次打开的工作标签，不再先显示环境概览；
- Codex、Claude Code、Pi Agent、OpenCode、Shell、VS Code 与 Preview 都是同级内嵌标签；
- 最左侧 Activity Bar 提供 Agent、Code、Preview、Shell、Import、Accounts 与 Settings 快速入口；
- 每个工具按 Workspace + 标签 ID 只创建一个稳定 iframe，并挂载在共享 View Layer；普通标签切换和主/辅面板交换只改变位置与可见性，不移动或重建 iframe；
- 最多保留 8 个挂载视图，超过上限时按 LRU 卸载最久未使用的隐藏面板，但对应 Agent/tmux 继续运行；
- 支持主工作区与辅助工作区双栏分屏，可以一边运行 Agent、一边编辑或查看网页；
- 标签、活动面板、分屏状态和最近工具会按 Workspace 保存在浏览器；
- 支持工具重新加载、新窗口打开、沉浸全屏和在另一侧打开；
- 文件可以直接拖到整个工作台并导入 `/workspace`；
- 新增命令面板，使用 `Ctrl/Cmd + K` 快速打开 Session、编辑器、Preview 与设置；
- Workspace 启停、账号管理和环境信息退到边栏、状态栏及弹层；
- Preview 代理会移除阻止同源工作台嵌入的 `X-Frame-Options` 和 CSP `frame-ancestors` 指令。

VibeDock 不会在标签切换时主动断开已挂载工具，但浏览器后台冻结、移动系统休眠或外层反向代理的 WebSocket idle timeout 仍可能中断连接。远程部署应为 `/w/` 路径配置足够长的 WebSocket idle timeout；断线后可使用工作台 Reload 重新连接。

工作台的核心使用方式：

```text
Agent Session + Preview
Agent Session + VS Code
VS Code + Shell
两个 Agent 并行
```

Preview 仍要求容器内服务监听 `0.0.0.0`。部分依赖根路径绝对资源或强绑定独立域名的应用，仍可能需要应用自身配置 base path。

## 定制环境与首页

首页是运行概览而不是营销页：它显示 Workspace 数量、运行中环境、活跃 Session、需要处理的状态，并提供最近 Session 恢复、项目导入、账号管理和定制环境入口。首页和 Workspace 工作台都支持亮色/暗色模式，选择会保存在当前浏览器。

定制环境是基于内置模板的可复用配置档，不允许提交任意 Dockerfile 或替换 VibeDock 基础镜像。配置档可以保存：

- APT 包；
- npm 全局模块；
- Python 包（安装到独立的 `/home/coder/.venvs/vibedock`）；
- 非敏感环境变量；
- 以 `coder` 身份从 `/workspace` 执行的初始化脚本；
- 创建 Workspace 时使用的 CPU、内存默认值。

创建 Workspace 时可以选择固定模板或定制环境；创建请求仍可以覆盖资源和普通环境变量。安装与初始化发生在项目 Git/文件导入后，已有 Workspace 不会因为定制环境被编辑或删除而改变。配置档不会保存明显的 API Key、Token、Secret 或 Password 变量。

## 0.4 新功能：Pi Agent 与 OpenCode

- 新增 Pi Agent Session；
- 新增 OpenCode Session；
- 两者不进入统一账号中心，也不共享账号登录状态；
- Provider、Base URL、模型、API 协议、API Key/Token 均按 Session 配置；
- Pi 支持 Thinking 参数和项目批准参数；
- OpenCode 支持自动模式参数；
- 每个 Pi/OpenCode Session 使用独立配置目录、缓存和会话状态；
- API Key/Token 使用与账号中心相同的 AES-256-GCM 主密钥加密；
- Session JSON 和前端只显示 `has_secret` 与末四位提示，不返回明文；
- 运行时生成 `models.json` 或 `opencode.json`，配置文件只引用临时环境变量，不写入明文密钥；
- Session 删除时同步删除其加密密钥记录。

## 0.3 能力：统一账号与自定义 API

Codex 与 Claude Code 保留独立的账号管理模型：

- 顶层“账号与 API”管理页；
- 原生 CLI 登录档案可跨 Workspace 复用；
- 支持 API Key、Bearer Token，以及 Claude setup-token / OAuth Token；
- 支持自定义 Base URL 和默认模型；
- 每个 Session 可单独绑定或切换账号；
- 密钥使用 AES-256-GCM 加密保存；
- Session 启动时才解密并通过 `docker exec` 标准输入注入；
- 账号仍被 Session 引用时禁止删除；
- 提供尽力而为的 Provider 连接测试。

Pi Agent 和 OpenCode 不使用这里的 AccountProfile。它们的配置只属于具体 Session。

## 0.2 基础能力

- 所有入口统一到控制面端口 `8787`；
- `/w/{workspace}/code/` 反向代理 VS Code Web；
- `/w/{workspace}/terminal/{session}/` 反向代理独立 Agent Session；
- `/w/{workspace}/port/{port}/` 预览容器中的开发服务；
- 一个 Workspace 可并行运行多个 Agent 或 Shell Session；
- Session 支持创建、改名、切换 Agent/配置、重启、删除和最近输出；
- 创建时或运行中导入 Git、ZIP 与文件；
- Workspace 停止后项目、Home 和 Agent 状态继续保留；
- 可选控制面 HTTP Basic Auth。

## 快速启动

要求：

- Linux 主机；
- Docker Engine；
- Docker Compose v2；
- 建议至少 4 GB 空闲内存和 10 GB 磁盘。

运行：

```bash
cp .env.example .env
# 远程部署时务必设置 VIBEDOCK_ADMIN_PASSWORD
./scripts/bootstrap.sh
```

打开：

```text
http://服务器地址:8787
```

首次启动会构建 Workspace 镜像，其中预装 VS Code Web、tmux、ttyd、Codex、Claude Code、Pi Agent 和 OpenCode。后续创建环境主要只需要启动容器、挂载卷和导入项目。

## Agent 认证模型

### Codex / Claude Code：托管账号

Codex 和 Claude Code 使用顶层 AccountProfile：

```text
AccountProfile
├─ Agent 类型
├─ 原生登录 / API Key / Bearer / OAuth
├─ Base URL
├─ 默认模型
├─ encrypted secret
└─ shared native-login directory
```

同一账号可以被多个 Workspace 与 Session 复用。

### Pi Agent / OpenCode：Session 参数

Pi Agent 和 OpenCode 没有 VibeDock 账号对象。创建或编辑 Session 时直接填写：

```text
Provider ID
Model
Base URL
Protocol
API Key / Token
Thinking（Pi）
Auto approve / auto mode
```

这些参数只影响当前 Session：

```text
Workspace
├─ Pi Session A ───── relay-a / model-a / key-a
├─ Pi Session B ───── relay-b / model-b / key-b
└─ OpenCode Session ─ relay-c / model-c / key-c
```

修改参数会重启目标 Session，但不会影响同 Workspace 的其他 Session。

## Pi Agent Session

创建 Session 时选择 `Pi Agent`，可填写：

- Provider ID：例如 `openai`、`anthropic`、`relay`；
- 模型：Provider 中的模型 ID；
- Base URL：自定义 API 地址；
- 协议：OpenAI Chat Completions、OpenAI Responses 或 Anthropic Messages；
- API Key / Token；
- Thinking：`off`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`；
- 信任当前项目参数。

每个 Pi Session 使用独立目录：

```text
/home/coder/.vibedock/agents/<session-id>/pi/
├─ models.json
└─ sessions/
```

`models.json` 中只保存对 `VIBEDOCK_DIRECT_API_KEY` 的引用，不保存密钥明文。

## OpenCode Session

创建 Session 时选择 `OpenCode`，可填写：

- Provider ID；
- 模型；
- Base URL；
- API 协议；
- API Key / Token；
- 自动模式。

每个 OpenCode Session 使用独立目录：

```text
/home/coder/.vibedock/agents/<session-id>/opencode/
├─ opencode.json
├─ data/
├─ cache/
└─ config/
```

VibeDock 根据协议生成 Provider adapter 配置，并通过 OpenCode 的环境变量插值引用临时密钥。

## Codex 与 Claude Code 账号中心

### Codex：使用 ChatGPT 官方设备登录

1. 打开“账号与 Provider”；
2. 点击“使用 ChatGPT 登录 Codex”；
3. 已有原生登录档案会直接开始；没有时先创建一个 `网页登录 / 官方 CLI` 档案；
4. 在弹窗中复制设备代码并打开服务端验证过的 OpenAI 官方 HTTPS 页面；
5. 完成后等待 Codex 官方 app-server 确认账号状态。

VibeDock 使用 Codex 0.144.6 的结构化设备授权协议。它不会索取 ChatGPT 密码，也不会把设备代码拼进 URL。普通 localhost OAuth 回调不适用于远程 Workspace，因此不作为后备方案。

登录状态保存在：

```text
/vibedock/credentials/<account-id>/codex
```

### Codex：API Key 或自定义端点

账号可填写：

```text
Auth type: API Key / Bearer Token
Base URL: https://api.openai.com/v1
Model: 可选
Secret: API Key 或 Token
```

VibeDock 会为该账号生成独立 `CODEX_HOME/config.toml`。当前自定义 Provider 使用 Responses wire API，因此第三方 Base URL 需要兼容相应接口。

### Claude Code：使用 Claude.ai 官方登录

在“账号与 Provider”点击“使用 Claude.ai 登录 Claude”。VibeDock 会打开一个与普通 Agent Session 分离的受保护登录终端，其中运行官方 `claude auth login --claudeai`。用户按官方 CLI 提示打开 Anthropic 页面，再把 `authorizationCode#state` 授权响应粘贴回官方 CLI；VibeDock 不实现 Anthropic 回调，不解析授权响应，也不接收 Claude.ai 密码。完成状态由 `claude auth status --json` 验证。

状态保存在：

```text
/vibedock/credentials/<account-id>/claude
```

### Claude Code：API / 网关

支持：

- Anthropic API Key；
- Bearer Token；
- `claude setup-token` 产生的 OAuth Token；
- 自定义 `ANTHROPIC_BASE_URL`；
- 默认模型。

### Workspace 默认模式

Codex/Claude Session 不选择托管账号时，沿用 Workspace Home 中已有的官方 CLI 登录状态或项目环境变量。Workspace 默认模式不会清理已有 `ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`CLAUDE_CODE_OAUTH_TOKEN` 或 `ANTHROPIC_BASE_URL`；绑定托管 Claude 账号时才隔离这些变量，避免两套身份互相污染。

### 登录入口与边界

- Codex：在“账号与 Provider”点击“使用 ChatGPT 登录 Codex”，通过官方设备页面登录；不需要先创建或找到普通 Agent Session；
- Claude：在同一页面点击“使用 Claude.ai 登录 Claude”，使用专用官方 CLI 登录终端；不需要运行 Workspace；
- 登录助手是短时固定命令容器，只挂载共享凭据卷；验证码、完整登录 URL、ttyd 内部密码和授权响应不写入状态文件、日志或浏览器存储；
- 登录先写入 staging 档案，只有官方 CLI 验证成功才替换稳定档案；失败、取消或过期的重新登录不会破坏已有有效档案；
- 关闭登录弹窗不会取消尝试；可以从账号卡片继续。取消、重试、重新登录和退出登录都有独立操作；
- Claude setup-token：先使用 Claude 官方 CLI 生成已有 Token，再保存到 VibeDock；这是 Token 导入，不是 Claude.ai 网页登录；VibeDock 不发起、不代理也不模拟第三方 OAuth；
- Pi / OpenCode：没有统一账号对象，必须在 Session 中显式填写 Provider ID、Base URL、模型、协议和 API Key / Token；不完整配置会被 API 拒绝，避免创建看似运行但实际不可用的 Session；
- Session 启动失败时，API 会保留 `status: error` 和错误信息，前端明确显示“已保存但未就绪”，不会提示普通成功。

## 密钥与主密钥

以下两类 Secret 均使用 AES-256-GCM 加密：

1. Codex/Claude AccountProfile Secret；
2. Pi/OpenCode Session Secret。

状态文件：

```text
data/workspaces.json
```

主密钥：

```text
data/credential.key
```

文件权限为 `0600`。也可以在 `.env` 中设置稳定的 32 字节密钥：

```bash
VIBEDOCK_CREDENTIAL_KEY=<base64url/base64/hex encoded 32-byte key>
```

**必须同时备份 `data/workspaces.json` 和 `data/credential.key`。** 丢失主密钥后，已有账号和 Session 密钥无法恢复，只能重新录入。

## 凭据注入过程

```text
Browser
  └─ sends account ID or Session parameters
       └─ VibeDock control plane
            ├─ decrypts secret in memory
            ├─ sends bootstrap script through docker exec stdin
            └─ Workspace /dev/shm/vibedock/sessions/<session>.env (0600)
                 └─ tmux starts selected Agent
```

Secret 不会作为 `docker exec -e TOKEN=...` 参数传入。Session 停止或重启时，其 `/dev/shm` 临时环境文件会被删除；容器停止后 tmpfs 内容也会消失。

## 统一工作台

所有用户入口位于同一 Origin：

```text
http://服务器:8787/workspace/<workspace-id>
http://服务器:8787/w/<workspace-id>/code/
http://服务器:8787/w/<workspace-id>/terminal/<session-id>/
http://服务器:8787/w/<workspace-id>/port/<port>/
```

Workspace 服务不发布随机宿主机端口。控制面和 Workspace 加入专用 Docker 网络，由 Go 反向代理 HTTP 与 WebSocket。

## 创建环境

1. 点击“创建环境”；
2. 选择模板、CPU、内存和首个 Session 类型；
3. Codex/Claude 选择托管账号，Pi/OpenCode 填写 Session 参数；
4. 填写 Git 地址，也可上传 ZIP 或多个文件；
5. 创建后直接进入 Workspace 工作台。

项目环境变量会进入 Docker 容器配置。模型密钥不要放在普通 Workspace 环境变量：Codex/Claude 放账号中心，Pi/OpenCode 放 Session 的加密 Secret 字段。

## 多 Session

每个 Session 使用：

```text
tmux session: vd-<session-id>
internal ttyd port: 7700-7999
```

Session 可以独立：

- 选择 Codex、Claude Code、Pi Agent、OpenCode 或 Shell；
- 选择托管账号或填写 Session Provider 参数；
- 改名；
- 重启；
- 删除；
- 查看最近输出。

切换 Agent、账号或参数只重启目标 Session。

## VS Code Web 与 Preview

VS Code：

```text
http://服务器:8787/w/<workspace-id>/code/?folder=/workspace
```

Preview：

```text
http://服务器:8787/w/<workspace-id>/port/3000/
```

应用需要监听 `0.0.0.0:<port>`。部分只支持根路径或绝对静态资源 URL 的应用可能不适合子路径代理。

## 数据持久化

每个 Workspace：

```text
vibedock-ws-<id>     -> /workspace
vibedock-home-<id>   -> /home/coder
```

统一 Codex/Claude 原生登录档案：

```text
vibedock-credentials -> /vibedock/credentials
```

控制面状态与加密密钥：

```text
data/workspaces.json
data/credential.key
```

Pi/OpenCode 的非敏感配置与本地 Session 数据位于 Workspace Home 卷；它们的加密 API Key 位于控制面状态文件的独立 `session_secrets` 映射中。

## Workspace 镜像与 CLI 更新策略

2026-07-19 的镜像基线使用精确版本：

| 组件 | 版本 | 安装来源 |
|---|---:|---|
| Node.js | 22.23.1 | 官方 Node Bookworm 镜像，固定 digest |
| code-server | 4.129.0 | 官方 GitHub `.deb`，按架构校验 SHA-256 |
| Codex | 0.144.6 | npm `@openai/codex` |
| Claude Code | 2.1.215 | npm `@anthropic-ai/claude-code` |
| Pi | 0.80.10 | npm `@earendil-works/pi-coding-agent` |
| OpenCode | 1.18.3 | npm `opencode-ai` |
| ttyd | 1.7.7 | 官方 GitHub binary，按架构校验 SHA-256 |

镜像中的测试基线安装在 root-owned `/opt/vibedock-cli`。`coder` 不能修改该目录，也没有无密码 sudo。交互式 shell、code-server 和 Agent Session 使用：

```text
NPM_CONFIG_PREFIX=/home/coder/.local
PATH=/home/coder/.local/bin:/opt/vibedock-cli/node_modules/.bin:...
```

因此用户显式运行：

```bash
npm install -g @openai/codex
```

会写入当前 Workspace 的持久 Home，而不是重命名 `/usr/local` 中的 root-owned package，不再依赖整体 `chown` 或提权。用户 overlay 会优先于镜像基线；若要回到基线，可删除对应的 `~/.local` package/link，或保留卷重建容器。容器启动时不会联网自动升级。

### 从早期 0.x 镜像升级

覆盖代码后执行：

```bash
docker build -t vibedock/workspace:local ./workspace
docker compose up -d --build
```

这只更新镜像和控制面。已经存在的 Workspace 容器仍保留创建时的镜像层，不会被静默替换。升级单个旧 Workspace 时必须先核对 Workspace ID、容器名、环境、资源限制、网络和挂载，然后：

1. 停止旧容器；
2. 记录并保留 `vibedock-ws-<id>`、`vibedock-home-<id>` 与共享 `vibedock-credentials` 挂载；
3. 仅删除旧容器，**不要删除或 purge 卷**；
4. 使用新镜像和原配置创建替代容器；
5. 启动后重新 reconcile Session，并检查 code-server、ttyd、stats 与账号状态。

当前 MVP 尚未提供“一键保留卷重建 Workspace”按钮。执行手工替换前请提交 Git 或备份项目文件；若容器配置与预期不一致，应停止操作并先调查。

## 配置

`.env`：

```bash
VIBEDOCK_PORT=8787
VIBEDOCK_DOCKER_NETWORK=vibedock
VIBEDOCK_ADMIN_USER=vibe
VIBEDOCK_ADMIN_PASSWORD=change-this-password
VIBEDOCK_CREDENTIAL_KEY=
VIBEDOCK_AUTH_HELPER_IMAGE=vibedock/workspace:local
VIBEDOCK_LOGIN_TTL=15m
VIBEDOCK_MAX_LOGIN_HELPERS=3
```

`VIBEDOCK_ADMIN_PASSWORD` 为空时不启用控制面认证，只应在本机或可信私网中使用。

## API

```text
GET    /api/health
GET    /api/templates
GET    /api/environment-profiles
POST   /api/environment-profiles
PATCH  /api/environment-profiles/{profile}
DELETE /api/environment-profiles/{profile}

GET    /api/accounts
POST   /api/accounts
PATCH  /api/accounts/{account}
POST   /api/accounts/{account}/probe
GET    /api/accounts/{account}/auth-status
POST   /api/accounts/{account}/login-attempts
GET    /api/accounts/{account}/login-attempts/{attempt}
DELETE /api/accounts/{account}/login-attempts/{attempt}
POST   /api/accounts/{account}/logout
DELETE /api/accounts/{account}

GET    /api/workspaces
POST   /api/workspaces
GET    /api/workspaces/{id}
GET    /api/workspaces/{id}/stats
POST   /api/workspaces/{id}/start
POST   /api/workspaces/{id}/stop
POST   /api/workspaces/{id}/files
DELETE /api/workspaces/{id}?purge=true

GET    /api/workspaces/{id}/sessions
POST   /api/workspaces/{id}/sessions
PATCH  /api/workspaces/{id}/sessions/{session}
POST   /api/workspaces/{id}/sessions/{session}/restart
GET    /api/workspaces/{id}/sessions/{session}/transcript
DELETE /api/workspaces/{id}/sessions/{session}
```

`GET /api/workspaces/{id}/stats` 返回当前 Workspace 的 Docker 实时采样、实际生效的容器限制和 VibeDock 配置配额。停止或缺失的容器返回 `usage: null`；该接口不代表宿主机剩余 CPU/内存，也不会用 Workspace 配额推算主机可用资源。

创建 Pi Session：

```json
{
  "name": "Pi relay",
  "agent": "pi",
  "direct_config": {
    "provider": "relay",
    "base_url": "https://example.com/v1",
    "model": "gpt-example",
    "protocol": "openai-responses",
    "thinking": "high",
    "auto_approve": true,
    "secret": "sk-..."
  }
}
```

创建 OpenCode Session：

```json
{
  "name": "OpenCode relay",
  "agent": "opencode",
  "direct_config": {
    "provider": "relay",
    "base_url": "https://example.com/v1",
    "model": "gpt-example",
    "protocol": "openai-completions",
    "auto_approve": false,
    "secret": "sk-..."
  }
}
```

更新 Session 时，`secret` 留空会保留已有密钥；`clear_secret: true` 会清除。响应不会包含明文 Secret。

## 常用命令

```bash
make bootstrap
make build-workspace
make build
make up
make down
make logs
make test
```

## 安全边界

这仍是可信单机或可信小团队内网 MVP，不是公共多租户平台。

主要风险：

- 控制面挂载 Docker socket，等价于宿主机 Docker 管理权限；
- Workspace 不提供无密码 sudo，但交互式 Shell、IDE 与 Agent 仍可执行当前 `coder` 用户有权执行的任意代码；
- 普通 Docker 容器不是不可信租户的强隔离边界；
- `vibedock-credentials` 共享挂载到所有 Workspace，且 Workspace 与登录助手使用共同 UID 1001；这是可信单用户共享，不是租户隔离；
- 原生 CLI 登录文件由 OpenAI/Anthropic 官方 CLI 管理，不经过 VibeDock 的 AES-256-GCM 字段加密；
- 同一 Workspace 内的 Shell、IDE 与 Agent 以相同 Unix 用户运行，技术上可读取正在运行 Session 的 `/dev/shm` 凭据文件；
- Pi/OpenCode 的加密 Secret 归 Session 所有，但运行时仍会进入该 Workspace 的进程环境；
- 尚未实现用户 ACL、Secret scope、审计、配额、网络策略、镜像签名和恶意上传扫描；
- Basic Auth 必须配合 HTTPS，不能保护明文 HTTP 链路上的密钥输入。

远程部署至少应：

1. 设置强 `VIBEDOCK_ADMIN_PASSWORD`；
2. 在外层配置 HTTPS；
3. 备份并限制 `data/credential.key` 权限；
4. 仅允许可信用户访问；
5. 不向其他容器暴露 Docker socket。

## 已验证

发布前执行：

```text
go test ./...
go vet ./...
go build ./cmd/server
node --check cmd/server/web/app.js
bash -n workspace/*.sh scripts/*.sh
```

另外使用 runner fixtures 验证了 Pi/OpenCode 最终启动参数、`models.json` / `opencode.json` 结构以及配置文件不包含密钥明文。发布流程还应构建真实 Workspace 镜像，并验证 code-server、ttyd、四个 Agent CLI、非 root Codex 更新、共享 iframe View Layer 和浏览器分屏切换。

第三方账号登录和 Provider API 是否可用取决于用户自己的账号、网络与端点；没有真实凭据时只能验证 onboarding、注入边界和结构化错误，不能声称外部认证已经通过。

更多设计说明见 `docs/ARCHITECTURE.md`。
