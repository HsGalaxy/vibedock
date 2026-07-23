#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

command -v docker >/dev/null || { echo "Docker is required." >&2; exit 1; }
docker compose version >/dev/null || { echo "Docker Compose v2 is required." >&2; exit 1; }

if [[ ! -f .env ]]; then
  cp .env.example .env
fi

read_dotenv_value() {
  local key="$1"
  awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      line = $0
      sub("^[[:space:]]*" key "[[:space:]]*=[[:space:]]*", "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line ~ /^".*"$/ || line ~ /^\047.*\047$/) {
        line = substr(line, 2, length(line) - 2)
      }
      value = line
    }
    END { print value }
  ' .env
}

resolve_setting() {
  local key="$1"
  local fallback="$2"
  local value
  if [[ -v "$key" ]]; then
    value="${!key}"
  else
    value="$(read_dotenv_value "$key")"
  fi
  printf '%s' "${value:-$fallback}"
}

is_loopback_address() {
  case "$1" in
    127.*|::1|'[::1]') return 0 ;;
    *) return 1 ;;
  esac
}

bind_address="$(resolve_setting VIBEDOCK_BIND_ADDRESS 127.0.0.1)"
port="$(resolve_setting VIBEDOCK_PORT 8787)"
preview_port="$(resolve_setting VIBEDOCK_PREVIEW_PORT 8788)"
auth_file="$(resolve_setting VIBEDOCK_AUTH_FILE /data/admin.json)"
bootstrap_token_file="$(resolve_setting VIBEDOCK_BOOTSTRAP_TOKEN_FILE /data/bootstrap.token)"
trust_proxy_headers="$(resolve_setting VIBEDOCK_TRUST_PROXY_HEADERS false)"
auth_file_relative=''
auth_file_host=''
administrator_initialized=''
if [[ "$auth_file" == /data/* ]]; then
  auth_file_relative="${auth_file#/data/}"
  if [[ -n "$auth_file_relative" && "$auth_file_relative" != */ ]]; then
    case "/$auth_file_relative/" in
      */./*|*/../*) ;;
      *) auth_file_host="data/$auth_file_relative" ;;
    esac
  fi
fi
if [[ -n "$auth_file_host" && -f "$auth_file_host" && -s "$auth_file_host" ]]; then
  administrator_initialized=1
fi
if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then
  printf 'VIBEDOCK_PORT must be an integer between 1 and 65535, got %q.\n' "$port" >&2
  exit 1
fi
if [[ ! "$preview_port" =~ ^[0-9]+$ ]] || (( preview_port < 1 || preview_port > 65535 )); then
  printf 'VIBEDOCK_PREVIEW_PORT must be an integer between 1 and 65535, got %q.\n' "$preview_port" >&2
  exit 1
fi
if [[ "$preview_port" == "$port" ]]; then
  printf 'VIBEDOCK_PORT and VIBEDOCK_PREVIEW_PORT must use different ports.\n' >&2
  exit 1
fi
if ! is_loopback_address "$bind_address"; then
  if [[ "$auth_file" != /data/* ]]; then
    printf 'A remote bind requires VIBEDOCK_AUTH_FILE to use persistent /data storage.\n' >&2
    exit 1
  fi
  if [[ -z "$auth_file_relative" || "$auth_file_relative" == */ ]]; then
    printf 'VIBEDOCK_AUTH_FILE must name a file below /data.\n' >&2
    exit 1
  fi
  if [[ -z "$auth_file_host" ]]; then
    printf 'VIBEDOCK_AUTH_FILE may not traverse outside /data.\n' >&2
    exit 1
  fi
  if [[ -z "$administrator_initialized" ]]; then
    printf '\nRefusing VIBEDOCK_BIND_ADDRESS=%s before administrator registration.\n' "$bind_address" >&2
    printf 'Use 127.0.0.1, register through a local browser or SSH tunnel, then configure the HTTPS proxy.\n' >&2
    exit 1
  fi
  printf '\nWARNING: VIBEDOCK_BIND_ADDRESS=%s exposes the control plane beyond this host.\n' "$bind_address" >&2
  case "$trust_proxy_headers" in
    1|true|TRUE|yes|YES|on|ON)
      printf 'Ensure only the trusted HTTPS proxy can reach ports %s/%s and that it overwrites forwarded headers.\n' "$port" "$preview_port" >&2
      ;;
    *)
      printf 'Use both ports only on a trusted LAN/VPN; public access requires an HTTPS reverse proxy.\n' >&2
      ;;
  esac
fi

./scripts/preflight.sh

printf '\n[1/3] Building prewarmed workspace image...\n'
docker build -t vibedock/workspace:local ./workspace
printf '\n[2/3] Building VibeDock control plane...\n'
docker compose build
printf '\n[3/3] Starting VibeDock...\n'
docker compose up -d

container_id="$(docker compose ps --quiet vibedock)"
if [[ -z "$container_id" ]]; then
  printf 'VibeDock did not create a control-plane container.\n' >&2
  docker compose logs --tail=80 vibedock >&2 || true
  exit 1
fi

healthy=''
for _ in {1..45}; do
  status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)"
  if [[ "$status" == healthy ]]; then
    healthy=1
    break
  fi
  if [[ "$status" == exited || "$status" == dead ]]; then
    break
  fi
  sleep 2
done
if [[ -z "$healthy" ]]; then
  printf 'VibeDock failed its startup health check. Recent logs:\n' >&2
  docker compose logs --tail=80 vibedock >&2 || true
  exit 1
fi

display_address="$bind_address"
if [[ "$display_address" == '::1' || "$display_address" == '[::1]' ]]; then
  display_address='[::1]'
elif ! is_loopback_address "$display_address"; then
  display_address='<server-address>'
fi
printf '\nVibeDock is ready: http://%s:%s\n' "$display_address" "$port"
if [[ -n "$administrator_initialized" ]]; then
  printf 'Sign in with the existing administrator account.\n'
else
  bootstrap_token="$(docker compose exec -T vibedock cat "$bootstrap_token_file" 2>/dev/null || true)"
  if [[ -z "$bootstrap_token" ]]; then
    if [[ -f "$auth_file_host" && -s "$auth_file_host" ]]; then
      printf 'The administrator account was created while bootstrap was finishing; sign in to continue.\n'
    else
      printf 'VibeDock started but the one-time bootstrap token could not be read from %s.\n' "$bootstrap_token_file" >&2
      printf 'Inspect the control-plane logs before attempting registration.\n' >&2
      exit 1
    fi
  else
    printf '\nInitial administrator setup is required.\n'
    printf '1. Open http://%s:%s\n' "$display_address" "$port"
    printf '2. Enter this one-time bootstrap code: %s\n' "$bootstrap_token"
    printf '3. Choose the administrator username and password.\n'
  fi
fi
printf 'VS Code and terminals use the authenticated control origin on :%s.\n' "$port"
printf 'Workspace previews use the isolated authenticated origin on :%s.\n' "$preview_port"
