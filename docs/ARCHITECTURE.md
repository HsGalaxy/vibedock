# VibeDock 0.5 Architecture


## Workspace-first Workbench

0.5 的浏览器端不再把 Workspace 详情作为主页面，而是把运行工具作为第一等对象：

```text
Workbench
├─ Activity Bar
├─ Session Sidebar
├─ Global Tool Tabs
├─ Primary Pane
├─ Optional Secondary Pane
└─ Status Bar / Settings dialogs
```

工具标签由稳定 ID 描述：

```text
home
code
session:<session-id>
port:<container-port>
```

每个 Pane 独立选择一个标签。布局状态保存在浏览器 `localStorage`，后端仍只维护 Workspace、Session 和账号事实状态。每个 Workspace + 标签 ID 对应一个稳定 iframe，统一挂载在 `pane-view-layer`；Pane 只提供几何位置，切换标签或交换分屏时通过 CSS 坐标和可见性复用 browsing context，不在两个 Pane DOM 之间 reparent iframe。最多保留 8 个 View，隐藏 View 超限时按 LRU 卸载。

iframe 通过现有同源代理加载：

```text
/w/<workspace>/code/
/w/<workspace>/terminal/<session>/
/w/<workspace>/port/<port>/
```

Preview 响应在代理边界移除 `X-Frame-Options`，并只删除 CSP 中的 `frame-ancestors` 指令，使用户自己运行的开发服务可以嵌入同源工作台；其余 CSP 指令保持不变。

## 产品对象

```text
AccountProfile                         DirectAgentConfig
├─ codex | claude                     ├─ pi | opencode Session only
├─ auth type                          ├─ provider / base URL / model
├─ encrypted secret                   ├─ protocol / thinking / auto mode
├─ base URL / model                   ├─ public metadata in Session
└─ shared native-login profile        └─ encrypted secret in session_secrets

Workspace
├─ template / image
├─ resource limits
├─ project volume
├─ home volume
├─ VS Code Web
├─ Session[]
│  ├─ agent type
│  ├─ AccountID OR DirectAgentConfig
│  ├─ tmux name
│  ├─ internal ttyd port
│  └─ lifecycle
└─ preview ports
```

Workspace 是长期开发环境；Session 是一次对话或工作任务。

认证分为两条路线：

1. Codex / Claude Code：使用可复用 AccountProfile；
2. Pi Agent / OpenCode：使用只属于 Session 的 DirectAgentConfig，不建立账号对象。

平台不重新实现 Agent loop，而是在独立 tmux 中运行原版 CLI。

## 控制面模块

```text
cmd/server
internal/app
  ├─ HTTP API
  ├─ Workspace / Session 编排
  ├─ AccountProfile 管理
  ├─ DirectAgentConfig 验证与解密
  ├─ 上传与导入
  └─ 反向代理
internal/model     Workspace / Session / Account / Runtime 模型
internal/docker    Docker Runtime、凭据注入、代理目标发现
internal/secrets   AES-GCM master key / encrypt / decrypt
internal/store     JSON 持久化与迁移
```

## Agent 类型矩阵

| Agent | 配置所有权 | 登录/认证 | Runtime 配置 |
|---|---|---|---|
| Codex | AccountProfile 或 Workspace 默认 | 原生登录、API Key、Bearer | `CODEX_HOME` / provider TOML |
| Claude Code | AccountProfile 或 Workspace 默认 | 原生登录、API Key、Bearer、OAuth | `CLAUDE_CONFIG_DIR` / Anthropic env |
| Pi Agent | Session | API Key/Token 参数 | per-Session `models.json` |
| OpenCode | Session | API Key/Token 参数 | per-Session `opencode.json` |
| Shell | 无 | 无 | interactive shell |

AccountID 与 DirectAgentConfig 互斥。Pi/OpenCode 提交非空 AccountID 会被 API 拒绝。

## AccountProfile

账号是控制面顶层资源，与 Workspace 分离：

```text
AccountProfile
  ID
  Name
  Agent
  AuthType
  BaseURL
  Model
  ExtraEnv (non-secret only)
  SecretCiphertext
  SecretHint
  LastUsedAt
```

HTTP API 返回 AccountView，刻意省略密文和明文 Secret。

### Codex Provider

托管 API 账号生成：

```toml
model = "..."
model_provider = "vibedock"

[model_providers.vibedock]
name = "VibeDock managed provider"
base_url = "https://.../v1"
wire_api = "responses"
env_key = "VIBEDOCK_PROVIDER_TOKEN"
```

配置写入账号专属 `CODEX_HOME/config.toml`，Secret 只存在于 Session 运行环境中。

### Claude Provider

Runner 根据认证类型设置：

```text
ANTHROPIC_API_KEY
ANTHROPIC_AUTH_TOKEN
CLAUDE_CODE_OAUTH_TOKEN
ANTHROPIC_BASE_URL
```

默认模型通过 CLI 参数传入。未绑定 AccountProfile 时是 Workspace 默认模式，runner 保留 Workspace 已有的 Anthropic 凭据和 Base URL；绑定托管账号后才清理冲突变量，并只设置所选认证方式对应的变量。VibeDock 不托管第三方 OAuth 页面；`login` 账号使用上文的 PKCE 编排，`oauth_token` 则只接收用户通过 Claude 官方工具获得的 setup-token。

## DirectAgentConfig

Pi/OpenCode Session 的公开配置嵌入 Session：

```text
DirectAgentConfig
  Provider
  BaseURL
  Model
  Protocol
  Thinking
  AutoApprove
  HasSecret
  SecretHint
```

明文或密文 Secret 不放进 Session 对象。Store 使用独立映射：

```json
{
  "session_secrets": {
    "<session-id>": "v1:<AES-GCM ciphertext>"
  }
}
```

这样 Workspace/Session API 可以完整返回配置，但不会意外序列化密钥。

### 协议归一化

接受并归一化为：

```text
openai-completions
openai-responses
anthropic-messages
```

Base URL 只接受 HTTP/HTTPS。Provider ID 只允许字母、数字、点、下划线和短横线。当前 Pi/OpenCode 接入要求 Provider ID、Base URL、Model 和加密 Secret 全部存在；控制面不提供一个虚假的默认 Provider，也不会把不完整配置创建成看似运行的 Session。

## Pi Agent Runtime

每个 Session 创建独立目录：

```text
$HOME/.vibedock/agents/<session-id>/pi
├─ models.json
└─ sessions/
```

Runner 设置：

```text
PI_CODING_AGENT_DIR=<session root>
VIBEDOCK_DIRECT_API_KEY=<runtime only>
```

生成的 `models.json` 使用环境变量引用：

```json
{
  "providers": {
    "relay": {
      "apiKey": "$VIBEDOCK_DIRECT_API_KEY",
      "baseUrl": "https://example.com/v1",
      "api": "openai-responses",
      "models": [{"id": "model-id", "name": "model-id"}]
    }
  }
}
```

启动参数按配置组合：

```text
pi
  --session-dir <dir>
  --provider <provider>
  --model <model>
  --thinking <level>
  --approve
```

`--approve` 仅在 DirectAgentConfig.AutoApprove 为 true 时添加。

## OpenCode Runtime

每个 Session 创建独立目录并隔离 XDG 状态：

```text
$HOME/.vibedock/agents/<session-id>/opencode
├─ opencode.json
├─ data/
├─ cache/
└─ config/
```

Runner 设置：

```text
OPENCODE_CONFIG=<session root>/opencode.json
XDG_DATA_HOME=<session root>/data
XDG_CACHE_HOME=<session root>/cache
XDG_CONFIG_HOME=<session root>/config
```

生成的 Provider 配置通过环境变量插值读取 Secret：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "relay": {
      "name": "VibeDock session provider",
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "apiKey": "{env:VIBEDOCK_DIRECT_API_KEY}",
        "baseURL": "https://example.com/v1"
      },
      "models": {
        "model-id": {"name": "model-id"}
      }
    }
  },
  "model": "relay/model-id"
}
```

协议到 adapter 的当前映射：

```text
openai-completions -> @ai-sdk/openai-compatible
openai-responses   -> @ai-sdk/openai
anthropic-messages -> @ai-sdk/anthropic
```

AutoApprove 为 true 时，启动参数增加 `--auto`。

## 加密存储

`internal/secrets` 使用 32 字节主密钥和 AES-GCM：

```text
plaintext secret
  -> random nonce
  -> AES-256-GCM seal
  -> v1:<base64url nonce+ciphertext>
```

主密钥来源优先级：

1. `VIBEDOCK_CREDENTIAL_KEY`；
2. `VIBEDOCK_CREDENTIAL_KEY_FILE`；
3. 自动生成并写入 `/data/credential.key`，权限 `0600`。

Account Secret 和 Direct Session Secret 共用同一 Keyring，但存储位置分离。

## Session 凭据解析

### Managed Account Session

1. 从 Store 读取 AccountProfile；
2. 验证 Account Agent 与 Session Agent 一致；
3. 在控制面内存中解密 Secret；
4. 构造 `SessionRuntimeRequest.Account`。

### Direct Agent Session

1. 从 Session 读取 DirectAgentConfig；
2. 从 `session_secrets` 读取对应密文；
3. 在控制面内存中解密；
4. 构造 `SessionRuntimeRequest.Direct`。

### 公共注入流程

1. Runtime 通过 `docker exec -i ... bash -s` 的 stdin 发送 bootstrap；
2. Workspace 生成 `/dev/shm/vibedock/sessions/<id>.env`，权限 `0600`；
3. tmux source 该文件并启动 `vibedock-agent-runner`；
4. Session 删除或重启时移除临时文件。

不使用 `docker exec -e SECRET=...`，避免 Secret 出现在控制面错误信息或进程参数中。

## 官方网页登录编排

网页登录由独立的 `AccountAuthManager` 与 `AuthRuntime` 管理，不依赖普通 Workspace/Session 生命周期：

```text
Account Center
  ├─ Codex -> official authorize URL -> paste localhost callback
  │                                    └─ one-shot helper -> token exchange -> auth.json
  └─ Claude -> official authorize URL -> paste code#state
                                       └─ one-shot helper -> token exchange -> .credentials.json
```

- 每个账号最多一个活动尝试，全局最多三个 helper；默认 15 分钟过期；
- 活动索引只存在于控制面内存，helper 的公开状态和私有 PKCE session 位于 credential volume，以便控制面重启后恢复；授权码、PKCE verifier、Provider token 和凭据内容不会进入 Store、日志或浏览器存储；
- helper 只能使用固定 Workspace image、命令、凭据卷和资源限制；浏览器不能传入 image、command、mount、network、port、环境变量、Provider URL 或 Docker 参数；
- Codex 对齐 0.144.6 官方 browser PKCE 参数和 credential schema；只接受 `auth.openai.com/oauth/authorize`，只接受精确的 localhost callback，并校验 OAuth state；
- Claude 对齐 2.1.215 的 PKCE 参数和 credential schema；只接受 `claude.com/cai/oauth/authorize`，并校验用户粘贴的 `authorizationCode#state`；
- start、inspect、cancel、status 使用无网络的一次性 helper；只有 complete 的固定 token exchange 操作允许访问 Provider 网络，且 token endpoint 不允许重定向；
- 每次登录写入 `.auth-attempts/<account>/<attempt>` staging，state 校验和 token 兑换成功后只原子替换认证文件；用户配置与 CLI 运行数据保持原位，取消、失败或过期不会覆盖已有稳定凭据；
- 账号更新/删除采用 rename-to-quarantine、Store mutation、commit/rollback 三阶段清理，启动 reconcile 恢复未完成事务；
- API 响应使用 `no-store`；mutation 需要 JSON、自定义请求头和 same-origin 检查。

升级 Codex 或 Claude Code 时，必须同步核对 OAuth client contract、官方 URL allowlist、token exchange 和 credential schema fixture；不兼容时应 fail closed。

## 原生登录档案

Codex/Claude 新 Workspace 挂载：

```text
vibedock-credentials:/vibedock/credentials
```

账号目录：

```text
/vibedock/credentials/<account-id>/codex
/vibedock/credentials/<account-id>/claude
```

Pi/OpenCode 不使用该卷作为账号目录，它们使用 Workspace Home 卷中的 per-Session 目录。

## 统一代理

所有用户入口位于同一个 Origin：

```text
/w/<workspace>/code/
/w/<workspace>/terminal/<session>/
/w/<workspace>/port/<port>/
```

控制面使用 Go `httputil.ReverseProxy` 转发 HTTP 和 WebSocket。Workspace 不通过随机宿主机端口暴露服务。

### VS Code

代理删除 `/w/<workspace>/code` 前缀，发往容器 `:8080`，并处理 WebSocket、Location、Cookie Path 与转发头。

### ttyd Session

每个 Session 分配容器内部端口 `7700-7999`，并设置 base path：

```text
/w/<workspace>/terminal/<session>
```

### Preview

Preview 删除 `/w/<workspace>/port/<port>` 前缀后转发，支持 WebSocket，但不能自动修复目标应用产生的绝对 URL。

## Docker 网络与卷

```text
VibeDock control plane
        │
        └── Docker network: vibedock
                 ├── workspace A
                 ├── workspace B
                 └── workspace C
```

新 Workspace 的挂载：

```text
vibedock-ws-<id>       /workspace
vibedock-home-<id>     /home/coder
vibedock-credentials   /vibedock/credentials
```

Runtime 删除 Workspace 时只清理前两个卷。

## Session 生命周期

创建 Session：

```text
allocate ID and ttyd port
validate agent/auth model
normalize AccountID or DirectAgentConfig
encrypt and persist new Secret
resolve runtime credentials
write tmpfs env file
create tmux vd-<session-id>
start selected Agent / Shell
start ttyd
persist metadata
```

`EnsureSession` 是幂等的：

- tmux 已存在时不重建 Agent；
- ttyd 丢失时只恢复 Web Terminal；
- 代理访问前再次 Ensure，提供基本自愈。

切换 Agent、AccountID 或 DirectAgentConfig 只重启目标 Session。

删除 Direct Agent Session 时同步删除 `session_secrets[session-id]`。

## Store

当前仍采用单 JSON 文件：

```json
{
  "workspaces": [],
  "accounts": [],
  "session_secrets": {}
}
```

所有 Secret 值均是 AES-GCM ciphertext。该设计适合 MVP，但多进程、高可用和大量 Workspace 前应迁移 PostgreSQL，并对 Secret、Account、Session 与用户所有权建立独立表。

## 安全模型

当前假设所有登录 VibeDock 的操作者彼此完全信任：

- Workspace 拥有交互式终端，但镜像不安装 sudo，也不授予 `coder` 无密码提权；
- Codex/Claude Workspace 与 auth helper 共享 Credential volume，并使用共同 UID 1001；这是可信单用户边界，不是租户隔离；
- 原生 credential files 由登录 helper 创建、由对应 CLI 消费和刷新，不经过 VibeDock AES-GCM field encryption；
- 控制面 Basic Auth 关闭时，任何能访问可信网络控制面的人都可能启动登录或删除本地凭据；UI 会持续显示这一边界；
- 同容器内 Session 使用同一 `coder` UID；
- Docker socket 使控制面具备宿主机级能力；
- 已获得任一 Workspace Shell 的用户可读取共享 Credential volume 中所有托管 Codex/Claude 账号文件；还可读取同一 Workspace 正在运行 Session 的 `/dev/shm` 凭据文件。当前部署模型因此只适用于完全互信的单用户或小团队。

加密主要防御：

- State 文件被单独复制或泄漏；
- API 响应和前端意外展示；
- Docker 命令参数与控制面错误日志。

它不防御已经获得 Workspace Shell、控制面进程权限或 Docker 主机权限的攻击者。

## Workspace 镜像与升级边界

Workspace 镜像使用受控的精确版本构建。root-owned 基线位于 `/opt/vibedock-cli`，而 `coder` 的显式 npm 更新写入持久化 `/home/coder/.local` 并优先于基线。root entrypoint 使用固定系统 PATH 和绝对基础设施命令，绝不从用户可写的 `~/.local/bin` 解析 `gosu`、code-server、ttyd 或 runner。容器启动不执行联网自动升级。

仅升级控制面或重新构建 image 不会替换运行中的旧 Workspace。升级必须保留 `vibedock-ws-<id>`、`vibedock-home-<id>` 和共享 credential mount，只替换容器，并复制原网络、资源限制、环境与标签。任何删除容器的操作前都要核对 Workspace ID 和挂载；镜像升级不得 purge 数据卷。

后续应增加“保留项目卷/Home 卷，按最新模板重建容器”的正式 API 和 UI。

## 后续演进

优先级建议：

1. 保留卷的一键重建 / 镜像升级；
2. PostgreSQL 与异步任务状态机；
3. 用户、组织、Workspace ACL 与审计；
4. 用户级 Secret Store 和账号授权范围；
5. GitHub/GitLab OAuth 与私有仓库凭据；
6. Snapshot / Fork / 保存为模板；
7. 依赖缓存与预热池；
8. 子域名 Preview；
9. Coder Runtime / 多节点调度；
10. gVisor、Kata 或 microVM 隔离。
