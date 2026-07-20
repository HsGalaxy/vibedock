#!/usr/bin/env bash
set -Eeuo pipefail

readonly system_path='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
readonly coder_path="/home/coder/.local/bin:/opt/vibedock-cli/node_modules/.bin:${system_path}"
export PATH="$system_path"

/usr/bin/mkdir -p \
  /workspace \
  /home/coder/.config \
  /home/coder/.local/bin \
  /home/coder/.local/lib \
  /home/coder/.local/share \
  /home/coder/.npm \
  /home/coder/.vibedock/sessions \
  /vibedock/credentials
/usr/bin/chown -R coder:coder /workspace /home/coder /vibedock/credentials

if [[ ! -f /home/coder/.bashrc ]]; then
  /usr/bin/touch /home/coder/.bashrc
fi
if ! /usr/bin/grep -Fq '# VibeDock managed shell environment' /home/coder/.bashrc; then
  /usr/bin/printf '%s\n' \
    '' \
    '# VibeDock managed shell environment' \
    'export NPM_CONFIG_PREFIX="$HOME/.local"' \
    'export PATH="$HOME/.local/bin:/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"' \
    "alias sessions='tmux list-sessions 2>/dev/null || true'" \
    'cd /workspace 2>/dev/null || true' \
    >>/home/coder/.bashrc
fi
/usr/bin/chown coder:coder /home/coder/.bashrc

/usr/sbin/gosu coder \
  /usr/bin/env \
    HOME=/home/coder \
    USER=coder \
    LOGNAME=coder \
    NPM_CONFIG_PREFIX=/home/coder/.local \
    PATH="$coder_path" \
  /usr/bin/code-server \
    --bind-addr 0.0.0.0:8080 \
    --auth none \
    --disable-telemetry \
    --disable-update-check \
    /workspace &
code_server_pid=$!

terminate() {
  /usr/bin/kill "$code_server_pid" 2>/dev/null || true
  wait "$code_server_pid" 2>/dev/null || true
}
trap terminate TERM INT

wait "$code_server_pid"
