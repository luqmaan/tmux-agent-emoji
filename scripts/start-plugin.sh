#!/usr/bin/env bash
set -euo pipefail

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/tmux-ai-status"
BINARY="$CACHE_DIR/tmux-ai-status-bin"
LOG_FILE="$CACHE_DIR/plugin.log"
LOCK_FILE="$CACHE_DIR/plugin.lock"
REPO_SLUG="${TMUX_AI_STATUS_REPO_SLUG:-luqmaan/tmux-ai-status}"
DOWNLOAD_BASE="https://github.com/${REPO_SLUG}/releases/latest/download"

mkdir -p "$CACHE_DIR"
touch "$LOG_FILE"

notify() { tmux display-message "$1" 2>/dev/null || true; }

download() {
	local url="$1"
	local dest="$2"

	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$dest"
		return
	fi
	if command -v wget >/dev/null 2>&1; then
		wget -qO "$dest" "$url"
		return
	fi
	echo "tmux-ai-status: need curl or wget to download release binaries" >>"$LOG_FILE"
	notify "tmux-ai-status: need curl or wget to download release binaries"
	return 1
}

binary_asset_name() {
	local os arch
	os="$(uname -s)"
	arch="$(uname -m)"

	case "$os" in
	Linux) ;;
	*)
		echo "unsupported-os"
		return 1
		;;
	esac

	case "$arch" in
	x86_64|amd64) echo "tmux-ai-status-linux-amd64" ;;
	aarch64|arm64) echo "tmux-ai-status-linux-arm64" ;;
	*)
		echo "unsupported-arch"
		return 1
		;;
	esac
}

socket_path="${1:-}"
if [ -z "$socket_path" ]; then
	socket_path="$(tmux display-message -p '#{socket_path}' 2>/dev/null || true)"
fi
if [ -z "$socket_path" ]; then
	exit 0
fi

exec 9>"$LOCK_FILE"
flock -w 10 9 || exit 0

needs_download=0
if [ ! -x "$BINARY" ]; then
	needs_download=1
fi
if [ -x "$BINARY" ] && [ "$CURRENT_DIR/tmux-ai-status.tmux" -nt "$BINARY" ]; then
	needs_download=1
fi
if [ -x "$BINARY" ] && [ "$CURRENT_DIR/scripts/start-plugin.sh" -nt "$BINARY" ]; then
	needs_download=1
fi
if [ -x "$BINARY" ] && [ -n "$(find "$BINARY" -mtime +1 -print -quit 2>/dev/null)" ]; then
	needs_download=1
fi

socket_key="$(printf '%s' "$socket_path" | cksum | awk '{print $1}')"
pid_file="$CACHE_DIR/${socket_key}.pid"

downloaded=0
if [ "$needs_download" -eq 1 ]; then
	asset_name="$(binary_asset_name)" || {
		echo "tmux-ai-status: unsupported platform $(uname -s)/$(uname -m)" >>"$LOG_FILE"
		notify "tmux-ai-status: unsupported platform $(uname -s)/$(uname -m)"
		exit 0
	}
	tmp_binary="$(mktemp "$CACHE_DIR/tmux-ai-status-bin.XXXXXX")"
	if ! download "${DOWNLOAD_BASE}/${asset_name}" "$tmp_binary" >>"$LOG_FILE" 2>&1; then
		rm -f "$tmp_binary"
		if [ ! -x "$BINARY" ]; then
			notify "tmux-ai-status: download failed, see $LOG_FILE"
			exit 0
		fi
	else
		chmod +x "$tmp_binary"
		mv "$tmp_binary" "$BINARY"
		downloaded=1
	fi
fi

if [ -f "$pid_file" ]; then
	pid="$(cat "$pid_file" 2>/dev/null || true)"
	if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
		if [ "$downloaded" -eq 0 ]; then
			exit 0
		fi
		kill "$pid" 2>/dev/null || true
		sleep 0.1
	fi
	rm -f "$pid_file"
fi

TMUX_AI_STATUS_SOCKET="$socket_path" nohup "$BINARY" >>"$LOG_FILE" 2>&1 &
echo $! >"$pid_file"
