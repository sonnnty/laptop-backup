#!/usr/bin/env bash
# install.sh: install laptop-backup for the current user on this laptop.
# Every step is idempotent and reports what it did; safe to re-run after
# pulling a new version.
#
# Settings come from, highest first: environment variables, then
# install.local.env next to this script (gitignored; see
# install.local.env.example), then the defaults below.
#   ./install.sh --print-config   show the resolved settings and exit
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_ENV="$REPO_DIR/install.local.env"
BIN="$HOME/.local/bin/laptop-backup"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/laptop-backup"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT=laptop-backup.service
PORT=8095

ok()   { printf '  [ok]   %s\n' "$*"; }
skip() { printf '  [skip] %s\n' "$*"; }
warn() { printf '  [warn] %s\n' "$*" >&2; }
step() { printf '\n==> %s\n' "$*"; }
die()  { printf '  [fail] %s\n' "$*" >&2; exit 1; }

# ask QUESTION: yes/no prompt, default yes; non-interactive means yes.
ask() {
	local reply
	[[ -t 0 ]] || return 0
	read -rp "  $1 [Y/n] " reply
	[[ -z $reply || $reply =~ ^[Yy] ]]
}

# env_value KEY FILE: read KEY=value from a .env file without sourcing it.
env_value() {
	local v
	v=$(sed -n -E "s/^[[:space:]]*(export[[:space:]]+)?$1[[:space:]]*=[[:space:]]*//p" "$2" | tail -n 1 | tr -d '\r')
	# Unquoted values may end with an inline "  # comment".
	if [[ $v != [\"\']* ]]; then
		v=${v%%[[:space:]]#*}
		v=${v%"${v##*[![:space:]]}"}
	fi
	v=${v%\"}; v=${v#\"}; v=${v%\'}; v=${v#\'}
	printf '%s' "$v"
}

# has_key KEY FILE: whether FILE assigns KEY (possibly to "").
has_key() {
	grep -Eq "^[[:space:]]*(export[[:space:]]+)?$1[[:space:]]*=" "$2"
}

# expand_home VALUE: expand a leading ~ or $HOME, since install.local.env
# is read as plain text, not by the shell.
expand_home() {
	local v=$1
	case $v in
		"~") v=$HOME ;;
		"~/"*) v=$HOME/${v#"~/"} ;;
		'$HOME' | '${HOME}') v=$HOME ;;
		'$HOME/'*) v=$HOME/${v#'$HOME/'} ;;
		'${HOME}/'*) v=$HOME/${v#'${HOME}/'} ;;
	esac
	printf '%s' "$v"
}

# setting NAME DEFAULT: an environment variable named NAME wins (even when
# set to ""), then NAME in install.local.env, then DEFAULT.
setting() {
	local name=$1 default=$2
	[[ -n ${!name+set} ]] && return 0
	if [[ -f $LOCAL_ENV ]] && has_key "$name" "$LOCAL_ENV"; then
		printf -v "$name" '%s' "$(expand_home "$(env_value "$name" "$LOCAL_ENV")")"
	else
		printf -v "$name" '%s' "$default"
	fi
}

# nginx conf.d directory of a local reverse proxy; empty = skip the proxy
# and /etc/hosts steps (the UI stays on http://127.0.0.1:8095).
setting PROXY_CONF_DIR ""
# Docker container running that nginx (reloaded after copying the config).
setting PROXY_CONTAINER local-proxy
# Host name served by the proxy and added to /etc/hosts.
setting PROXY_HOST backup
# Optional .env file to import GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET from.
setting IMPORT_ENV ""
# Image used to build when Go is not installed.
setting GO_IMAGE golang:1.26

if [[ -n $PROXY_HOST && ! $PROXY_HOST =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
	die "PROXY_HOST must be a plain host name, got: $PROXY_HOST"
fi

case "${1:-}" in
	"") ;;
	--print-config)
		for k in PROXY_CONF_DIR PROXY_CONTAINER PROXY_HOST IMPORT_ENV GO_IMAGE; do
			printf '%s=%s\n' "$k" "${!k}"
		done
		exit 0
		;;
	*) die "usage: $0 [--print-config]" ;;
esac

json_escape() {
	local s=${1//\\/\\\\}
	printf '%s' "${s//\"/\\\"}"
}

[[ $(uname -s) == Linux ]] || die "laptop-backup runs on Linux only"

step "1/6 Build the binary"
build_cmd="CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/laptop-backup ./cmd/laptop-backup"
# Each build runs as an if condition so a failure (e.g. a system Go older
# than 1.26) falls through to the next way instead of aborting under set -e.
built=""
if command -v go >/dev/null 2>&1; then
	if (cd "$REPO_DIR" && eval "$build_cmd"); then
		built="with $(go version | awk '{print $3}')"
	else
		warn "go build failed with $(go version | awk '{print $3}') (need >= 1.26); trying Docker or a prebuilt binary"
	fi
fi
if [[ -z $built ]] && command -v docker >/dev/null 2>&1; then
	if docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
		-v "$REPO_DIR":/src -w /src "$GO_IMAGE" sh -c "$build_cmd"; then
		built="in Docker ($GO_IMAGE)"
	else
		warn "Docker build failed"
	fi
fi
if [[ -n $built ]]; then
	ok "built $built"
elif [[ -x $REPO_DIR/bin/laptop-backup ]]; then
	warn "could not build: using the existing $REPO_DIR/bin/laptop-backup"
else
	die "need Go >= 1.26 or Docker to build (or copy a prebuilt bin/laptop-backup here)"
fi
if cmp -s "$REPO_DIR/bin/laptop-backup" "$BIN"; then
	skip "$BIN is already up to date"
else
	install -Dm755 "$REPO_DIR/bin/laptop-backup" "$BIN"
	ok "installed $BIN"
fi

step "2/6 Default config"
mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
if [[ -f $CONFIG_DIR/config.json ]]; then
	skip "$CONFIG_DIR/config.json exists (kept)"
else
	"$BIN" -init >/dev/null
	ok "wrote $CONFIG_DIR/config.json"
fi

step "3/6 Google OAuth client (credentials.json)"
cred="$CONFIG_DIR/credentials.json"
if [[ -s $cred ]]; then
	skip "$cred exists (kept)"
else
	id="" secret=""
	if [[ -n $IMPORT_ENV && ! -f $IMPORT_ENV ]]; then
		warn "IMPORT_ENV=$IMPORT_ENV not found; asking for the client instead"
	elif [[ -n $IMPORT_ENV ]]; then
		id=$(env_value GOOGLE_CLIENT_ID "$IMPORT_ENV")
		secret=$(env_value GOOGLE_CLIENT_SECRET "$IMPORT_ENV")
		# Only the client id/secret are imported, never another app's refresh token.
		if [[ -z $id || -z $secret ]]; then
			warn "$IMPORT_ENV has no GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET"
		elif ! ask "Import GOOGLE_CLIENT_ID/SECRET from $IMPORT_ENV?"; then
			id="" secret=""
		fi
	fi
	if [[ (-z $id || -z $secret) && -t 0 ]]; then
		read -rp "  GOOGLE_CLIENT_ID: " id
		read -rsp "  GOOGLE_CLIENT_SECRET: " secret
		echo
	fi
	if [[ -n $id && -n $secret ]]; then
		(umask 077 && printf '{\n  "client_id": "%s",\n  "client_secret": "%s"\n}\n' \
			"$(json_escape "$id")" "$(json_escape "$secret")" >"$cred")
		chmod 600 "$cred"
		ok "wrote $cred (mode 0600)"
	else
		warn "no OAuth client configured; re-run install.sh or create $cred by hand (see README)"
	fi
fi

step "4/6 systemd user service"
if ! systemctl --user show-environment >/dev/null 2>&1; then
	warn "systemctl --user is not available in this session; start $BIN manually"
else
	if cmp -s "$REPO_DIR/deploy/$UNIT" "$UNIT_DIR/$UNIT"; then
		skip "$UNIT_DIR/$UNIT is up to date"
	else
		install -Dm644 "$REPO_DIR/deploy/$UNIT" "$UNIT_DIR/$UNIT"
		systemctl --user daemon-reload
		ok "installed $UNIT_DIR/$UNIT"
	fi
	# The service accepts Host "backup" by default; another proxy host name
	# is passed through a drop-in so the unit file itself stays generic.
	dropin="$UNIT_DIR/$UNIT.d/host.conf"
	want="[Service]
Environment=LAPTOP_BACKUP_HOST=$PROXY_HOST"
	if [[ $PROXY_HOST == backup ]]; then
		if [[ -f $dropin ]]; then
			rm -f "$dropin"
			systemctl --user daemon-reload
			ok "removed $dropin (back to the default host name)"
		fi
	elif [[ -f $dropin && $(<"$dropin") == "$want" ]]; then
		skip "$dropin is up to date"
	else
		mkdir -p "$(dirname "$dropin")"
		printf '%s\n' "$want" >"$dropin"
		systemctl --user daemon-reload
		ok "wrote $dropin (host ${PROXY_HOST:-<none>})"
	fi
	if systemctl --user is-enabled --quiet "$UNIT"; then
		skip "$UNIT already enabled"
	else
		systemctl --user enable "$UNIT" >/dev/null
		ok "enabled $UNIT"
	fi
	# restart (not just start) so a rebuilt binary is picked up
	systemctl --user restart "$UNIT"
	ok "(re)started $UNIT"
fi

step "5/6 nginx proxy"
if [[ -z $PROXY_CONF_DIR ]]; then
	skip "PROXY_CONF_DIR is not set; to serve the UI as http://$PROXY_HOST through a local nginx, set PROXY_CONF_DIR (and PROXY_CONTAINER) in install.local.env"
elif [[ -z $PROXY_HOST ]]; then
	skip "PROXY_HOST is empty"
elif [[ ! -d $PROXY_CONF_DIR ]]; then
	warn "$PROXY_CONF_DIR not found; copy deploy/backup.conf into the proxy's conf.d yourself"
else
	changed=0
	conf="$PROXY_CONF_DIR/backup.conf"
	rendered=$(sed "s/server_name backup;/server_name $PROXY_HOST;/" "$REPO_DIR/deploy/backup.conf")
	if [[ -f $conf && $(<"$conf") == "$rendered" ]]; then
		skip "$conf is up to date"
	else
		printf '%s\n' "$rendered" >"$conf"
		changed=1
		ok "wrote $conf (server_name $PROXY_HOST)"
	fi
	if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$PROXY_CONTAINER"; then
		warn "container $PROXY_CONTAINER is not running; reload it later with: docker exec $PROXY_CONTAINER nginx -s reload"
	elif [[ $changed == 0 ]] && docker exec "$PROXY_CONTAINER" nginx -T 2>/dev/null | grep -qF "server_name $PROXY_HOST;"; then
		skip "$PROXY_CONTAINER already serves $PROXY_HOST"
	elif docker exec "$PROXY_CONTAINER" nginx -t >/dev/null 2>&1; then
		docker exec "$PROXY_CONTAINER" nginx -s reload
		ok "reloaded $PROXY_CONTAINER"
	else
		warn "nginx -t failed in $PROXY_CONTAINER; not reloading (check: docker exec $PROXY_CONTAINER nginx -t)"
	fi
fi

step "6/6 /etc/hosts entry"
if [[ -z $PROXY_CONF_DIR || -z $PROXY_HOST ]]; then
	skip "no proxy configured (needs PROXY_CONF_DIR and PROXY_HOST); nothing to add"
elif grep -Eq "^[[:space:]]*127\.0\.0\.1[[:space:]]+([^#]*[[:space:]])?${PROXY_HOST//./\\.}([[:space:]]|#|$)" /etc/hosts; then
	skip "/etc/hosts already maps $PROXY_HOST"
else
	echo "127.0.0.1   $PROXY_HOST" | sudo tee -a /etc/hosts >/dev/null
	ok "added '127.0.0.1   $PROXY_HOST' to /etc/hosts (sudo)"
fi

step "Check"
up=0
for _ in 1 2 3 4 5; do
	if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; then
		up=1
		break
	fi
	sleep 1
done
if [[ $up == 1 ]]; then
	ok "service answers on http://127.0.0.1:$PORT"
else
	warn "service not answering yet; see: journalctl --user -u $UNIT"
fi

url="http://127.0.0.1:$PORT"
[[ -n $PROXY_CONF_DIR && -n $PROXY_HOST ]] && url="http://$PROXY_HOST"
printf '\nDone. Open %s → ตั้งค่า → "เชื่อมต่อบัญชี Google", set the Drive folder ID, then add folders.\n' "$url"
