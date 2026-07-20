#!/usr/bin/env bash
set -Eeuo pipefail

export NPM_CONFIG_PREFIX="${NPM_CONFIG_PREFIX:-$HOME/.local}"
export PATH="$HOME/.local/bin:/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

agent="${AGENT:-shell}"
account_id="${VIBEDOCK_ACCOUNT_ID:-}"
auth_type="${VIBEDOCK_AUTH_TYPE:-workspace}"
permission_mode="${VIBEDOCK_PERMISSION_MODE:-manual}"
dangerous_skip="${VIBEDOCK_DANGEROUS_SKIP:-false}"
base_url="${VIBEDOCK_BASE_URL:-}"
model="${VIBEDOCK_MODEL:-}"
secret="${VIBEDOCK_PROVIDER_TOKEN:-}"

direct_provider="${VIBEDOCK_DIRECT_PROVIDER:-vibedock}"
direct_base_url="${VIBEDOCK_DIRECT_BASE_URL:-}"
direct_model="${VIBEDOCK_DIRECT_MODEL:-}"
direct_protocol="${VIBEDOCK_DIRECT_PROTOCOL:-openai-completions}"
direct_thinking="${VIBEDOCK_DIRECT_THINKING:-}"
permission_policy="${VIBEDOCK_PERMISSION_POLICY:-ask}"
direct_auto_approve="${VIBEDOCK_DIRECT_AUTO_APPROVE:-false}"
direct_secret="${VIBEDOCK_DIRECT_API_KEY:-}"
session_id="${SESSION_ID:-session}"
launch_command_json="${VIBEDOCK_LAUNCH_COMMAND_JSON:-}"
config_mode="${VIBEDOCK_CONFIG_MODE:-managed}"
custom_config_b64="${VIBEDOCK_CUSTOM_CONFIG_B64:-}"

load_compiled_command() {
  local -n target=$1
  [[ -n "$launch_command_json" && "$launch_command_json" != "{}" ]] || return 1
  mapfile -t target < <(python3 - "$launch_command_json" <<'PY'
import json
import sys
try:
    value = json.loads(sys.argv[1])
    executable = value["executable"]
    arguments = value["arguments"]
    if not isinstance(executable, str) or not isinstance(arguments, list):
        raise ValueError
    print(executable)
    for argument in arguments:
        if not isinstance(argument, str) or any(ord(char) < 32 for char in argument):
            raise ValueError
        print(argument)
except Exception:
    raise SystemExit(2)
PY
  )
  [[ ${#target[@]} -gt 0 ]] || return 1
  for index in "${!target[@]}"; do
    target[index]="${target[index]/\$HOME/$HOME}"
  done
  return 0
}

if [[ -n "$account_id" ]]; then
  profile_root="/vibedock/credentials/${account_id}"
  mkdir -p "$profile_root"
  chmod 700 "$profile_root" 2>/dev/null || true
fi

run_codex() {
  local -a command=(codex)
  if [[ -n "$account_id" ]]; then
    export CODEX_HOME="/vibedock/credentials/${account_id}/codex"
    mkdir -p "$CODEX_HOME"
    chmod 700 "$CODEX_HOME" 2>/dev/null || true
  fi

  if [[ "$auth_type" == "api_key" || "$auth_type" == "bearer_token" ]]; then
    : "${secret:?This Codex account has no API token}"
    export VIBEDOCK_PROVIDER_TOKEN="$secret"
    export VIBEDOCK_CODEX_BASE_URL="${base_url:-https://api.openai.com/v1}"
    export VIBEDOCK_CODEX_MODEL="$model"
    python3 - <<'PY'
import json
import os
from pathlib import Path

home = Path(os.environ["CODEX_HOME"])
home.mkdir(parents=True, exist_ok=True)
base_url = os.environ["VIBEDOCK_CODEX_BASE_URL"].rstrip("/")
model = os.environ.get("VIBEDOCK_CODEX_MODEL", "").strip()
lines = []
if model:
    lines.append(f"model = {json.dumps(model)}")
lines.extend([
    'model_provider = "vibedock"',
    '',
    '[model_providers.vibedock]',
    'name = "VibeDock managed provider"',
    f"base_url = {json.dumps(base_url)}",
    'wire_api = "responses"',
    'env_key = "VIBEDOCK_PROVIDER_TOKEN"',
    '',
])
path = home / "vibedock.config.toml"
path.write_text("\n".join(lines), encoding="utf-8")
path.chmod(0o600)
PY
    command+=(--profile vibedock)
  elif [[ -n "$model" ]]; then
    command+=(--model "$model")
  fi

  load_compiled_command command || true

  "${command[@]}"
}

run_claude() {
  local -a command=(claude)
  if [[ -n "$account_id" ]]; then
    export CLAUDE_CONFIG_DIR="/vibedock/credentials/${account_id}/claude"
    mkdir -p "$CLAUDE_CONFIG_DIR"
    chmod 700 "$CLAUDE_CONFIG_DIR" 2>/dev/null || true
  fi

  if [[ -n "$account_id" ]]; then
    # Managed profiles are isolated from Workspace-level Claude credentials.
    unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_BASE_URL
    case "$auth_type" in
      api_key)
        : "${secret:?This Claude account has no API key}"
        export ANTHROPIC_API_KEY="$secret"
        ;;
      bearer_token)
        : "${secret:?This Claude account has no bearer token}"
        export ANTHROPIC_AUTH_TOKEN="$secret"
        ;;
      oauth_token)
        : "${secret:?This Claude account has no OAuth token}"
        export CLAUDE_CODE_OAUTH_TOKEN="$secret"
        ;;
    esac
    if [[ -n "$base_url" ]]; then
      export ANTHROPIC_BASE_URL="${base_url%/}"
    fi
  fi
  if [[ -n "$model" ]]; then
    command+=(--model "$model")
  fi
  if [[ "$agent" == "claude" && "$permission_mode" != "manual" ]]; then
    command+=(--permission-mode "$permission_mode")
  fi
  if [[ "$agent" == "claude" && "$dangerous_skip" == "true" ]]; then
    command+=(--allow-dangerously-skip-permissions --dangerously-skip-permissions)
  fi
  load_compiled_command command || true
  "${command[@]}"
}

run_pi() {
  local root="$HOME/.vibedock/agents/${session_id}/pi"
  local -a command=(pi --session-dir "$root/sessions")
  mkdir -p "$root/sessions"
  chmod 700 "$root" "$root/sessions" 2>/dev/null || true
  export PI_CODING_AGENT_DIR="$root"
  export VIBEDOCK_DIRECT_API_KEY="$direct_secret"
  export VIBEDOCK_DIRECT_PROVIDER="$direct_provider"
  export VIBEDOCK_DIRECT_BASE_URL="$direct_base_url"
  export VIBEDOCK_DIRECT_MODEL="$direct_model"
  export VIBEDOCK_DIRECT_PROTOCOL="$direct_protocol"

  if [[ "$config_mode" == "custom" ]]; then
    printf '%s' "$custom_config_b64" | base64 -d >"$root/models.json"
    chmod 600 "$root/models.json"
  else
  python3 - <<'PY'
import json
import os
from pathlib import Path

root = Path(os.environ["PI_CODING_AGENT_DIR"])
provider = os.environ.get("VIBEDOCK_DIRECT_PROVIDER", "vibedock").strip() or "vibedock"
base_url = os.environ.get("VIBEDOCK_DIRECT_BASE_URL", "").strip().rstrip("/")
model = os.environ.get("VIBEDOCK_DIRECT_MODEL", "").strip()
protocol = os.environ.get("VIBEDOCK_DIRECT_PROTOCOL", "openai-completions").strip() or "openai-completions"
entry = {"apiKey": "$VIBEDOCK_DIRECT_API_KEY"}
if base_url:
    entry["baseUrl"] = base_url
    entry["api"] = protocol
    if model:
        entry["models"] = [{"id": model, "name": model}]
path = root / "models.json"
path.write_text(json.dumps({"providers": {provider: entry}}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
path.chmod(0o600)
PY
  fi

  [[ -n "$direct_provider" ]] && command+=(--provider "$direct_provider")
  [[ -n "$direct_model" ]] && command+=(--model "$direct_model")
  [[ -n "$direct_thinking" ]] && command+=(--thinking "$direct_thinking")
  [[ "$permission_policy" == "auto_approve" || "$direct_auto_approve" == "true" ]] && command+=(--approve)
  load_compiled_command command || true
  "${command[@]}"
}

run_opencode() {
  local root="$HOME/.vibedock/agents/${session_id}/opencode"
  local config="$root/opencode.json"
  local -a command=(opencode)
  mkdir -p "$root/data" "$root/cache" "$root/config"
  chmod 700 "$root" "$root/data" "$root/cache" "$root/config" 2>/dev/null || true
  export XDG_DATA_HOME="$root/data"
  export XDG_CACHE_HOME="$root/cache"
  export XDG_CONFIG_HOME="$root/config"
  export OPENCODE_CONFIG="$config"
  export VIBEDOCK_DIRECT_API_KEY="$direct_secret"
  export VIBEDOCK_DIRECT_PROVIDER="$direct_provider"
  export VIBEDOCK_DIRECT_BASE_URL="$direct_base_url"
  export VIBEDOCK_DIRECT_MODEL="$direct_model"
  export VIBEDOCK_DIRECT_PROTOCOL="$direct_protocol"

  if [[ "$config_mode" == "custom" ]]; then
    printf '%s' "$custom_config_b64" | base64 -d >"$config"
    chmod 600 "$config"
  else
  python3 - <<'PY'
import json
import os
from pathlib import Path

provider = os.environ.get("VIBEDOCK_DIRECT_PROVIDER", "vibedock").strip() or "vibedock"
base_url = os.environ.get("VIBEDOCK_DIRECT_BASE_URL", "").strip().rstrip("/")
model = os.environ.get("VIBEDOCK_DIRECT_MODEL", "").strip()
protocol = os.environ.get("VIBEDOCK_DIRECT_PROTOCOL", "openai-completions").strip() or "openai-completions"
npm = "@ai-sdk/openai" if protocol == "openai-responses" else "@ai-sdk/anthropic" if protocol == "anthropic-messages" else "@ai-sdk/openai-compatible"
provider_entry = {
    "name": "VibeDock session provider",
    "options": {"apiKey": "{env:VIBEDOCK_DIRECT_API_KEY}"},
}
if base_url:
    provider_entry["npm"] = npm
    provider_entry["options"]["baseURL"] = base_url
if model:
    provider_entry["models"] = {model: {"name": model}}
config = {"$schema": "https://opencode.ai/config.json", "provider": {provider: provider_entry}}
if model:
    config["model"] = f"{provider}/{model}"
path = Path(os.environ["OPENCODE_CONFIG"])
path.write_text(json.dumps(config, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
path.chmod(0o600)
PY
  fi

  [[ -n "$direct_model" ]] && command+=(--model "${direct_provider}/${direct_model}")
  [[ "$permission_policy" == "auto_approve" || "$direct_auto_approve" == "true" ]] && command+=(--auto)
  command+=(/workspace)
  load_compiled_command command || true
  "${command[@]}"
}

case "$agent" in
  codex)
    printf '\033[1;32mVibeDock · Codex\033[0m\n'
    run_codex
    ;;
  claude)
    printf '\033[1;32mVibeDock · Claude Code\033[0m\n'
    run_claude
    ;;
  pi)
    printf '\033[1;32mVibeDock · Pi Agent\033[0m\n'
    run_pi
    ;;
  opencode)
    printf '\033[1;32mVibeDock · OpenCode\033[0m\n'
    run_opencode
    ;;
  *)
    printf '\033[1;32mVibeDock · Shell\033[0m\n'
    if [[ -n "$launch_command_json" && "$launch_command_json" != "{}" ]]; then
      local_dummy=()
      load_compiled_command local_dummy || true
      "${local_dummy[@]}"
    fi
    ;;
esac
