#!/usr/bin/env bash
set -Eeuo pipefail
session_id="${1:?session id is required}"
exec tmux attach-session -t "vd-${session_id}"
