# Agent Configuration and Launch Profiles

## Status

This document defines the target design for browser-managed Agent configuration
and launch commands in VibeDock. It is an additive extension of the current
Codex, Claude Code, Pi Agent, and OpenCode integrations.

The primary compatibility requirement is simple: an existing Workspace or
Session with none of the new fields must start exactly as it does today.

## Goals

VibeDock should make Agent configuration visible and controllable without
requiring users to enter a container terminal and discover provider-specific
files or command-line flags.

The feature has four parts:

1. View and edit supported Agent configuration files in the browser.
2. Offer graphical controls for common launch options.
3. Accept additional advanced CLI arguments without accepting a shell program.
4. Preview the exact redacted command that VibeDock will execute.

The preview and execution path must use the same backend command compiler. A
frontend-only preview is not authoritative and is not acceptable.

## Non-goals

- Exposing authentication files, OAuth tokens, API keys, logs, databases, or
  arbitrary files from an Agent profile.
- Providing a general-purpose web file manager for the host or containers.
- Executing user-supplied shell strings with `eval`, `sh -c`, redirections,
  pipelines, substitutions, or command chaining.
- Replacing the official Codex or Claude authentication flows.
- Changing existing Session behavior when no launch profile is configured.

## Configuration scopes

Configuration scope follows the ownership model of each Agent.

| Agent | Configuration scope | Runtime root | Authentication |
| --- | --- | --- | --- |
| Codex | Account | `CODEX_HOME` | Managed account or API credential |
| Claude Code | Account | `CLAUDE_CONFIG_DIR` | Managed account or API credential |
| Pi Agent | Session | `$HOME/.vibedock/agents/<session-id>/pi` | Session API key/token |
| OpenCode | Session | `$HOME/.vibedock/agents/<session-id>/opencode` | Session API key/token |

Codex and Claude configuration is shared by Sessions bound to the same account.
Launch options remain Session-scoped, so two Sessions may use the same login
with different models or other CLI flags.

Pi and OpenCode do not use `AccountProfile`. Their provider configuration,
custom configuration text, launch options, and encrypted secret belong to one
Session.

## Managed and custom configuration modes

Pi and OpenCode currently regenerate their configuration file during every
start. Browser editing would be misleading unless overwrite behavior is made
explicit. Each Session therefore has a configuration mode:

- `managed`: VibeDock generates the file from structured Provider fields. This
  is the default and preserves current behavior.
- `custom`: the browser-edited file is authoritative. VibeDock validates and
  materializes it without replacing the user's content.

The UI must show which mode is active. Switching from `custom` to `managed`
requires confirmation because the next start replaces the materialized custom
file. A reset action restores the versioned default template.

Custom Pi and OpenCode files must refer to the runtime secret through the
existing environment placeholder. Plaintext secrets are rejected:

- Pi: `$VIBEDOCK_DIRECT_API_KEY`
- OpenCode: `{env:VIBEDOCK_DIRECT_API_KEY}`

## Codex and Claude profile files

`CODEX_HOME` and `CLAUDE_CONFIG_DIR` are directories, not single configuration
files. The browser may present a small directory tree, but only through an
explicit provider-specific policy.

Each policy defines:

- allowed relative paths or path patterns;
- editable and read-only file types;
- maximum file and aggregate response sizes;
- parser and validation rules;
- protected and hidden paths.

Authentication and runtime state remain hidden. At minimum, the following
classes are never returned by the API:

- Codex `auth.json` and any token-bearing equivalent;
- Claude OAuth/session credential files;
- SQLite databases and journals;
- logs, transcripts, histories, caches, and temporary files;
- symlinks, devices, sockets, and files outside the configured profile root.

The initial supported editable files are deliberately narrow:

- Codex: `config.toml`;
- Claude Code: JSON settings files explicitly added to the Claude policy after
  compatibility verification against the pinned CLI version.

For Codex API-key or bearer-token accounts, VibeDock writes its provider
bridge to the hidden `vibedock.config.toml` profile and launches Codex with
`--profile vibedock`. The user-editable `config.toml` is therefore never
overwritten by a Session restart. Official-login accounts do not receive this
generated profile.

The policy can grow without turning the endpoint into arbitrary filesystem
access.

## File operations

Configuration file operations use a fixed helper command running against the
appropriate Docker volume. The control plane never accepts an absolute path.

Required operations:

- list the allowed file tree;
- read one allowed text file;
- validate proposed content;
- atomically write one allowed text file;
- restore the default or remove an optional override file.

Writes use a temporary file in the same directory followed by an atomic rename.
The previous accepted value is retained as one recoverable revision or in a
small bounded revision history. JSON and TOML must parse before a write is
committed. Provider-specific checks run after syntax validation where a safe
offline validation command exists.

Every request enforces:

- strict account, Workspace, Session, Agent, and relative-path validation;
- path containment after normalization;
- no symlink traversal;
- regular files only;
- UTF-8 text only;
- request, file, and directory-entry limits;
- no-store response headers.

## Storage model

### Codex and Claude Code

The existing `vibedock-credentials` volume remains the source of truth. The
configuration helper accesses only the selected account/provider root:

```text
/vibedock/credentials/<account-id>/codex
/vibedock/credentials/<account-id>/claude
```

No second copy of provider-managed profile state is introduced.

### Pi Agent and OpenCode

The control-plane state stores the canonical custom configuration text and its
mode as part of the Session's non-secret configuration. On start or restart,
the runtime materializes it into the existing per-Workspace Home volume:

```text
/home/coder/.vibedock/agents/<session-id>/pi/models.json
/home/coder/.vibedock/agents/<session-id>/opencode/opencode.json
```

A new Docker volume per Session is intentionally avoided. Workspace containers
are long-lived and Docker cannot attach another volume after container
creation. The existing Home volume already provides persistence and is scoped
to one Workspace.

Session secrets remain in the encrypted `session_secrets` store and are only
injected into the in-memory runtime environment.

## Launch profile model

All Agents use one Session-scoped launch profile. The conceptual model is:

```text
AgentLaunchProfile
  version
  structured options
  extra arguments[]
  configuration mode
  custom configuration text (Pi/OpenCode only)
```

The persisted API model must never contain a secret. Empty or absent profiles
mean legacy behavior.

Advanced arguments are stored as an array of tokens, not a shell command:

```json
["--model", "gpt-5.4", "--search", "-c", "feature=true"]
```

The API applies bounded item counts and token lengths, rejects NUL and control
characters, and does not allow changing the executable. Shell operators have
no special meaning because execution uses an argument vector.

## Command compiler

A backend command compiler accepts the Agent, current pinned CLI version,
structured options, extra arguments, and runtime paths. It returns:

```text
LaunchCommand
  executable
  arguments[]
  redacted environment summary
  warnings[]
```

The compiler is the only place that maps UI choices to CLI flags. Both the
preview endpoint and Session runtime call it.

Precedence is deterministic:

1. VibeDock-required arguments and paths;
2. structured UI options;
3. advanced arguments in the order supplied.

Provider-specific protected arguments, such as Pi's managed session directory
or OpenCode's Workspace path, cannot be overridden. Conflicting duplicate
flags produce a warning or validation error instead of silently diverging from
the preview.

The shell-form preview is presentation only. It uses correct quoting and
redacts secret-bearing environment variables. Execution uses the executable
and argument vector directly.

## Initial graphical options

The graphical surface starts with stable, high-value options and keeps the rest
under Advanced Arguments.

### Codex

- model override;
- approval policy;
- sandbox mode;
- web search toggle;
- additional `-c key=value` configuration overrides.

### Claude Code

- model override;
- permission mode;
- dangerous permission bypass with the existing explicit confirmation;
- resume/continue behavior where supported by the pinned CLI.

### Pi Agent

- Provider and model;
- thinking level;
- approval mode;
- managed/custom `models.json`.

### OpenCode

- Provider and model;
- automatic mode;
- managed/custom `opencode.json`.

The option schema is versioned with the pinned CLI compatibility manifest.
Flags are not assumed stable across provider upgrades.

## API surface

The exact paths may follow existing router conventions, but the capability set
is:

```text
GET  Session launch profile
PUT  Session launch profile
POST Session launch preview

GET  Account configuration tree
GET  Account configuration file
PUT  Account configuration file
POST Account configuration validation
POST Account configuration reset

GET  Session custom configuration
PUT  Session custom configuration
POST Session custom configuration validation
POST Session custom configuration reset
```

Mutations use the existing same-origin and VibeDock request-marker checks.
Changes that affect a running Session require an explicit restart action or a
request field that clearly asks VibeDock to restart it.

## User interface

The Session editor gains a `Launch` section:

- common options rendered with selects, toggles, and checkboxes;
- an Advanced Arguments editor using one argument per row;
- warnings for conflicting or unsupported flags;
- a read-only exact command preview with a refresh action;
- a clear indication that changes require restart.

Pi and OpenCode additionally get a `Configuration` view with a Managed/Custom
segmented control and JSON editor.

The account editor gains a `Configuration` view for Codex and Claude profiles.
It uses a compact directory tree and text editor rather than nested cards. The
UI must clearly mark protected files as unavailable instead of implying that
authentication data can be edited.

Editors provide validate, save, reset, and reload commands. They do not contain
secrets and do not render arbitrary HTML from file content.

## Compatibility and migration

- All new persisted fields use `omitempty`-style zero values.
- Existing state files load without migration work.
- An absent launch profile invokes the existing command builder unchanged.
- Existing managed Pi/OpenCode configuration remains the default.
- Existing Codex/Claude authentication directories remain valid.
- No Workspace container is recreated solely to adopt this feature.
- Switching account, Agent, or launch profile restarts only the affected
  Session under the existing restart rules.

## Test strategy

Backend tests cover:

- state compatibility and secret non-serialization;
- argument token validation and command compilation for every Agent;
- preview/execution equivalence;
- conflicting/protected flags;
- managed and custom configuration materialization;
- JSON/TOML validation;
- path traversal, symlink, oversized-file, binary-file, and protected-file
  rejection;
- account and Session ownership checks;
- no-store and same-origin mutation behavior.

Runner tests assert exact argument arrays, configuration file content, secret
placeholders, permissions, and per-Session paths.

Frontend tests cover option normalization, advanced argument editing, command
preview rendering, Managed/Custom switching, validation errors, and redaction.

End-to-end verification uses the pinned real CLI images to confirm that default
profiles preserve today's launch behavior and that custom profiles start the
intended Agent without exposing credentials.

## Delivery sequence

1. Add backward-compatible launch/configuration models and the command
   compiler with tests.
2. Route the existing runtime through the compiler without changing default
   commands.
3. Add Pi/OpenCode Managed/Custom configuration and preview APIs.
4. Add the Session launch/configuration UI.
5. Add the restricted Codex/Claude account configuration helper and API.
6. Add the account configuration file-tree UI.
7. Run real-image and browser end-to-end verification before release.
