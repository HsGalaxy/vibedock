#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

command -v docker >/dev/null || { echo "Docker is required." >&2; exit 1; }
docker compose version >/dev/null || { echo "Docker Compose v2 is required." >&2; exit 1; }

./scripts/preflight.sh

if [[ ! -f .env ]]; then
  cp .env.example .env
fi

printf '\n[1/3] Building prewarmed workspace image...\n'
docker build -t vibedock/workspace:local ./workspace
printf '\n[2/3] Building VibeDock control plane...\n'
docker compose build
printf '\n[3/3] Starting VibeDock...\n'
docker compose up -d

port="$(awk -F= '/^VIBEDOCK_PORT=/{print $2}' .env | tail -n1)"
port="${port:-8787}"
printf '\nVibeDock is ready: http://localhost:%s\n' "$port"
printf 'VS Code, terminals and previews are available below the same :%s origin.\n' "$port"
