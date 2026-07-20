#!/usr/bin/env bash
set -Eeuo pipefail
tmux list-sessions -F '#{session_name}\t#{session_windows}\t#{session_attached}' 2>/dev/null || true
