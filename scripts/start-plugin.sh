#!/usr/bin/env bash
set -euo pipefail

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/tmux-ai-status"
BINARY="$CACHE_DIR/tmux-ai-status-bin"
LOG_FILE="$CACHE_DIR/plugin.log"
LOCK_FILE="$CACHE_DIR/plugin.lock"

mkdir -p "$CACHE_DIR"
touch "$LOG_FILE"

notify() {
	tmux display-message "$1" 2>/dev/null || true
}

socket_path="${1:-}"
if [ -z "$socket_path" ]; then
	socket_path="$(tmux display-message -p '#{socket_path}' 2>/dev/null || true)"
fi
if [ -z "$socket_path" ]; then
	exit 0
fi

if ! command -v go >/dev/null 2>&1; then
	echo "tmux-ai-status: Go is required to build the plugin binary" >>"$LOG_FILE"
	notify "tmux-ai-status: Go is required to build the plugin binary"
	exit 0
fi

exec 9>"$LOCK_FILE"
flock -w 10 9 || exit 0

needs_build=0
if [ ! -x "$BINARY" ]; then
	needs_build=1
fi
for src in "$CURRENT_DIR/main.go" "$CURRENT_DIR/go.mod"; do
	if [ "$src" -nt "$BINARY" ]; then
		needs_build=1
		break
	fi
done

rebuilt=0
if [ "$needs_build" -eq 1 ]; then
	tmp_binary="$(mktemp "$CACHE_DIR/tmux-ai-status-bin.XXXXXX")"
	if ! (cd "$CURRENT_DIR" && go build -o "$tmp_binary" . >>"$LOG_FILE" 2>&1); then
		rm -f "$tmp_binary"
		notify "tmux-ai-status: build failed, see $LOG_FILE"
		exit 0
	fi
	mv "$tmp_binary" "$BINARY"
	chmod +x "$BINARY"
	rebuilt=1
fi

socket_key="$(printf '%s' "$socket_path" | cksum | awk '{print $1}')"
pid_file="$CACHE_DIR/${socket_key}.pid"

if [ -f "$pid_file" ]; then
	pid="$(cat "$pid_file" 2>/dev/null || true)"
	if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
		if [ "$rebuilt" -eq 0 ]; then
			exit 0
		fi
		kill "$pid" 2>/dev/null || true
		sleep 0.1
	fi
	rm -f "$pid_file"
fi

TMUX_AI_STATUS_SOCKET="$socket_path" nohup "$BINARY" >>"$LOG_FILE" 2>&1 &
echo $! >"$pid_file"
