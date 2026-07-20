# Changelog

## 0.5.0

- Rebuilt the frontend as a workspace-first cloud development desktop.
- Added embedded tabs for Agent Sessions, VS Code Web and container port previews.
- Added primary/secondary pane split view for Agent + Preview, Agent + VS Code and parallel sessions.
- Added per-Workspace browser layout persistence.
- Added immersive fullscreen, iframe reload, external open and send-to-other-pane actions.
- Added a command palette with Ctrl/Cmd+K navigation.
- Added workbench-wide drag-and-drop file import.
- Moved infrastructure controls into the activity bar, status bar and settings dialogs.
- Simplified the dashboard around quick creation and recent projects.
- Added preview response header rewriting for embedded development servers.
- Added tests preserving non-frame CSP directives while enabling workbench embedding.

## 0.4.0

- Added Pi Agent as a first-class Session agent.
- Added OpenCode as a first-class Session agent.
- Added per-Session provider, Base URL, model, protocol and API token configuration.
- Kept Pi/OpenCode outside the centralized AccountProfile model.
- Added encrypted Session-scoped secrets with independent deletion lifecycle.
- Added Pi per-Session `models.json` generation and isolated session storage.
- Added OpenCode per-Session `opencode.json` generation and isolated XDG storage.
- Added OpenAI Chat Completions, OpenAI Responses and Anthropic Messages protocol selection.
- Added Pi thinking and project approval parameters.
- Added OpenCode auto mode parameter.
- Added Pi/OpenCode controls to Workspace creation and Session edit dialogs.
- Added health capability flags for Pi, OpenCode and Session parameter authentication.
- Added runner behavior checks ensuring generated config files do not contain plaintext secrets.

## 0.3.0

- Added a centralized Codex / Claude Code account and provider hub.
- Added native CLI login profiles shared across Workspaces.
- Added API Key, Bearer Token and Claude OAuth/setup-token profiles.
- Added custom Base URL and default model per account.
- Added per-Session account selection and account switching.
- Added account selection when creating a Workspace's first Session.
- Added AES-256-GCM encryption for stored account secrets.
- Added persistent credential master key generation and configuration.
- Added best-effort provider connectivity probes.
- Added account reference protection before deletion.
- Added secret-safe Session bootstrap over docker exec stdin.
- Added tmpfs-backed per-Session runtime environment files.
- Added Codex custom Responses API provider generation.
- Added Claude Code environment mapping for Anthropic-compatible gateways.
- Added account management UI, status cards and responsive controls.
- Prevented deletion of the shared credential volume when deleting a Workspace.

## 0.2.0

- Added same-origin reverse proxy for VS Code, terminals and preview ports.
- Removed host random port publishing for new Workspaces.
- Added multi-Session Workspace model.
- Added independent tmux + ttyd lifecycle per Session.
- Added Session create, update, restart, delete and transcript APIs.
- Added file import for existing Workspaces.
- Added port preview UI.
- Rebuilt the frontend as a Workspace workbench.
- Added optional control-plane Basic Auth.
- Added automatic migration from the single-Agent state model.
