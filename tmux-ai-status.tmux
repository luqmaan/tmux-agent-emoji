#!/usr/bin/env bash
set -euo pipefail

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
START_SCRIPT="$CURRENT_DIR/scripts/start-plugin.sh"

if [ "$(tmux show-option -gqv @tmux-ai-status-hooks-installed)" != "1" ]; then
	tmux set-option -gq @tmux-ai-status-hooks-installed 1
	tmux set-hook -ag client-attached "run-shell -b \"$START_SCRIPT\""
	tmux set-hook -ag session-created "run-shell -b \"$START_SCRIPT\""
fi

tmux run-shell -b "$START_SCRIPT"
