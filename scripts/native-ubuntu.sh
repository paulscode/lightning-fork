#!/usr/bin/env bash
#
# Build and set up Lightning Fork on Ubuntu, next to a Bitcoin Knots node
# that runs natively (not in Docker) on the same machine.
#
# Read docs/native-ubuntu.md first. Every command this script runs is printed
# before it runs (lines starting with "+"), so you can also follow along and
# type the commands yourself.
#
#   scripts/native-ubuntu.sh check      look around, change nothing
#   scripts/native-ubuntu.sh deps       apt packages: git, make, curl (sudo)
#   scripts/native-ubuntu.sh go         Go toolchain into ~/.local/go
#   scripts/native-ubuntu.sh build      build lnd and lncli into ~/go/bin
#   scripts/native-ubuntu.sh configure  check Knots, write ~/.lnd/lnd.conf
#   scripts/native-ubuntu.sh tor        optional: reach (and be reached by)
#                                       peers over Tor
#   scripts/native-ubuntu.sh service    optional: a systemd user service
#   scripts/native-ubuntu.sh dashboard  optional: the web dashboard, with lnd
#                                       and it as systemd user services
#   scripts/native-ubuntu.sh all        deps, go, build, configure
#
# Settings (environment variables, all optional):
#   LND_DIR          lnd's data directory                  (~/.lnd)
#   LND_ALIAS        the name your node shows to others    (your hostname)
#   BITCOIN_CLI      how to call bitcoin-cli, with any options it needs,
#                    e.g. "bitcoin-cli -datadir=/data/bitcoin"   (bitcoin-cli)
#   BITCOIN_DIR      Knots data directory, for the .cookie file  (~/.bitcoin)
#   BITCOIND_RPCHOST Knots RPC address                     (127.0.0.1:8332)
#   BITCOIND_RPCUSER, BITCOIND_RPCPASS
#                    use these instead of the cookie file
#   GO_DIR           where to unpack Go                    (~/.local/go)
#   TOR_SOCKS, TOR_CONTROL  Tor proxy and control port (127.0.0.1:9050, :9051)
#   DASHBOARD_HOST   where the dashboard listens: 127.0.0.1 for this machine
#                    only, 0.0.0.0 for other devices on your network too
#                                                          (127.0.0.1)
#   DASHBOARD_PORT   its port                              (3006)
#   DASHBOARD_REF    the dashboard release to build        (see below)
#   MOBILE_PORT      a port for the Lightning Fork phone app to pair and
#                    connect over your local network, e.g. 3443  (off)

set -euo pipefail

# The Go release Lightning Fork's own release builds use (see Dockerfile),
# with the checksums go.dev publishes for it.
GO_VERSION=1.26.8
GO_SHA256_amd64=d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b
GO_SHA256_arm64=211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0

# The dashboard (github.com/paulscode/umbrel-lightning-fork) runs on the
# Node.js its Umbrel and StartOS image uses, with nodejs.org's checksums.
DASHBOARD_REPO=${DASHBOARD_REPO:-https://github.com/paulscode/umbrel-lightning-fork.git}
DASHBOARD_REF=${DASHBOARD_REF:-v1.3.2-blake2b.17.1}
NODE_VERSION=16.20.2
NODE_SHA256_x64=874463523f26ed528634580247f403d200ba17a31adf2de98a7b124c6eb33d87
NODE_SHA256_arm64=e88d86154d1ce53dc52fd74d79d4bfdf0b05f58c0bb2639adfa36e9378b770c4

# Block 961640, where the Bitcoin BLAKE2b chain begins. A Knots node on this
# chain reports this hash; a node on the SHA256 chain reports another one.
ACTIVATION_HEIGHT=961640
ACTIVATION_HASH=0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb

REPO_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LND_DIR=${LND_DIR:-$HOME/.lnd}
LND_ALIAS=${LND_ALIAS:-$(hostname -s)}
BITCOIN_CLI=${BITCOIN_CLI:-bitcoin-cli}
BITCOIN_DIR=${BITCOIN_DIR:-$HOME/.bitcoin}
BITCOIND_RPCHOST=${BITCOIND_RPCHOST:-127.0.0.1:8332}
GO_DIR=${GO_DIR:-$HOME/.local/go}
GOBIN_DIR=$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin
DASHBOARD_HOST=${DASHBOARD_HOST:-127.0.0.1}
DASHBOARD_PORT=${DASHBOARD_PORT:-3006}
LF_SHARE=$HOME/.local/share/lightning-fork   # the dashboard, its Node.js, its data
LF_CONF=$HOME/.config/lightning-fork         # its settings and passwords
UNIT_DIR=$HOME/.config/systemd/user

export PATH="$GO_DIR/bin:$GOBIN_DIR:$PATH"

say() { printf '\n== %s\n' "$*"; }
ok() { printf '   ok: %s\n' "$*"; }
warn() { printf '   NOTE: %s\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
run() { printf '+ %s\n' "$*"; "$@"; }
have() { command -v "$1" >/dev/null 2>&1; }
# root needs no sudo (and a fresh container has none)
SUDO=sudo
[ "$(id -u)" -ne 0 ] || SUDO=

# bitcoin-cli with the user's options; word splitting is intended.
bcli() { $BITCOIN_CLI "$@"; }

go_arch() {
	case "$(uname -m)" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported CPU $(uname -m): Lightning Fork builds here for x86_64 and arm64" ;;
	esac
}

go_ok() {
	have go || return 1
	local v
	v=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
	# go.mod asks for at least this version.
	local need
	need=$(sed -n 's/^go //p' "$REPO_DIR/go.mod")
	[ "$(printf '%s\n%s\n' "$need" "$v" | sort -V | head -1)" = "$need" ]
}

step_check() {
	say "Machine"
	printf '   %s, %s\n' "$(. /etc/os-release && echo "$PRETTY_NAME")" "$(uname -m)"
	for p in git make curl; do
		if have "$p"; then ok "$p"; else warn "$p missing (step: deps)"; fi
	done
	if go_ok; then ok "Go $(go env GOVERSION)"; else warn "no Go new enough (step: go)"; fi
	if [ -x "$GOBIN_DIR/lnd" ]; then
		ok "lnd built: $("$GOBIN_DIR/lnd" --version)"
	else
		warn "lnd not built yet (step: build)"
	fi
	check_knots || true
	if [ -f "$LND_DIR/lnd.conf" ]; then
		ok "$LND_DIR/lnd.conf exists"
	else
		warn "no $LND_DIR/lnd.conf yet (step: configure)"
	fi
}

step_deps() {
	say "Packages"
	local missing=()
	for p in git make curl ca-certificates unzip jq xz-utils openssh-client; do
		dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p")
	done
	if [ ${#missing[@]} -eq 0 ]; then
		ok "all installed already"
		return
	fi
	run $SUDO apt-get update
	run $SUDO apt-get install -y "${missing[@]}"
}

step_go() {
	say "Go $GO_VERSION"
	if go_ok; then
		ok "already have $(go env GOVERSION) at $(command -v go)"
		return
	fi
	local arch sum tarball
	arch=$(go_arch)
	case $arch in
	amd64) sum=$GO_SHA256_amd64 ;;
	arm64) sum=$GO_SHA256_arm64 ;;
	esac
	tarball="go$GO_VERSION.linux-$arch.tar.gz"
	local tmp
	tmp=$(mktemp -d)
	run curl -fsSL -o "$tmp/$tarball" "https://go.dev/dl/$tarball"
	printf '+ echo "%s  %s" | sha256sum -c\n' "$sum" "$tarball"
	(cd "$tmp" && echo "$sum  $tarball" | sha256sum -c --quiet) ||
		die "the download does not match Go's published checksum"
	run rm -rf "$GO_DIR"
	run mkdir -p "$(dirname "$GO_DIR")"
	run tar -C "$(dirname "$GO_DIR")" -xzf "$tmp/$tarball"
	if [ "$(basename "$GO_DIR")" != go ]; then
		run mv "$(dirname "$GO_DIR")/go" "$GO_DIR"
	fi
	rm -rf "$tmp"
	ok "$("$GO_DIR/bin/go" version)"
	GOBIN_DIR=$("$GO_DIR/bin/go" env GOPATH)/bin
	export PATH="$GO_DIR/bin:$GOBIN_DIR:$PATH"
	path_hint
}

path_hint() {
	if ! grep -qs "$GO_DIR/bin" "$HOME/.bashrc"; then
		warn "to find go, lnd and lncli in new terminals, add this to ~/.bashrc:"
		printf '     export PATH="%s/bin:%s:$PATH"\n' "$GO_DIR" "$GOBIN_DIR"
	fi
}

step_build() {
	say "Build"
	go_ok || die "Go is missing or too old; run: $0 go"
	have make || die "make is missing; run: $0 deps"
	cd "$REPO_DIR"
	local desc
	desc=$(git describe --tags --always 2>/dev/null || echo unknown)
	printf '   source: %s (%s)\n' "$REPO_DIR" "$desc"
	# A go.work file would point the build at local checkouts of the forked
	# libraries; a clone has none, and this keeps a stray one out.
	run env GOWORK=off make release-install
	ok "$("$GOBIN_DIR/lnd" --version)"
	ok "installed $GOBIN_DIR/lnd and $GOBIN_DIR/lncli"
	path_hint
}

# Fills RPC_COOKIE or RPC_USER/RPC_PASS, ZMQ_BLOCK/ZMQ_TX; returns non-zero
# with an explanation when Knots isn't usable yet.
check_knots() {
	say "Knots"
	local cli_first=${BITCOIN_CLI%% *}
	have "$cli_first" || {
		warn "$cli_first not found; set BITCOIN_CLI to the full path"
		return 1
	}
	local info
	info=$(bcli getblockchaininfo 2>&1) || {
		warn "bitcoin-cli can't reach Knots: $info"
		warn "is Knots running? does BITCOIN_CLI need -datadir or -conf?"
		return 1
	}
	local chain blocks headers
	chain=$(printf '%s' "$info" | sed -n 's/.*"chain": *"\([a-z0-9]*\)".*/\1/p')
	blocks=$(printf '%s' "$info" | sed -n 's/.*"blocks": *\([0-9]*\).*/\1/p')
	headers=$(printf '%s' "$info" | sed -n 's/.*"headers": *\([0-9]*\).*/\1/p')
	[ "$chain" = main ] || {
		warn "Knots is on '$chain'; this guide is for mainnet"
		return 1
	}
	# Knots before v29.4.1 follows the SHA256 chain. While a node is still
	# below the fork height, its version is the only thing that tells.
	local kv
	kv=$(bcli -version | head -1 | grep -o 'v[0-9][0-9]*\.[0-9][0-9]*\(\.[0-9][0-9]*\)\{0,1\}' | head -1 | tr -d v)
	case "$kv" in
	*.*.*) ;;
	*.*) kv=$kv.0 ;;
	esac
	if [ -z "$kv" ] || [ "$(printf '%s\n%s\n' 29.4.1 "$kv" | sort -V | head -1)" != 29.4.1 ]; then
		warn "$(bcli -version | head -1)"
		warn "Lightning Fork needs Bitcoin Knots v29.4.1 or later; older Knots follows the SHA256 chain. Upgrade Knots first: https://bitcoinknots.org"
		return 1
	fi
	ok "$(bcli -version | head -1), mainnet, at block $blocks of $headers"

	if [ "${blocks:-0}" -lt "$ACTIVATION_HEIGHT" ]; then
		warn "Knots is still syncing (block $blocks); Lightning Fork waits until it passes $ACTIVATION_HEIGHT"
	else
		local h
		h=$(bcli getblockhash "$ACTIVATION_HEIGHT")
		if [ "$h" != "$ACTIVATION_HASH" ]; then
			warn "block $ACTIVATION_HEIGHT is $h"
			warn "that is the SHA256 chain. Lightning Fork needs Knots v29.4.1 or later, on the Bitcoin BLAKE2b chain"
			return 1
		fi
		ok "on the Bitcoin BLAKE2b chain (block $ACTIVATION_HEIGHT matches)"
	fi

	# lnd talks to Knots over RPC itself, so check the credentials it will use,
	# not bitcoin-cli's.
	RPC_COOKIE='' RPC_USER='' RPC_PASS=''
	local auth
	if [ -n "${BITCOIND_RPCUSER:-}" ]; then
		RPC_USER=$BITCOIND_RPCUSER RPC_PASS=${BITCOIND_RPCPASS:-}
		auth="$RPC_USER:$RPC_PASS"
	elif [ -r "$BITCOIN_DIR/.cookie" ]; then
		RPC_COOKIE=$BITCOIN_DIR/.cookie
		auth=$(cat "$RPC_COOKIE")
	else
		warn "can't read $BITCOIN_DIR/.cookie (Knots writes it while running)"
		warn "set BITCOIN_DIR to Knots' data directory, or BITCOIND_RPCUSER/BITCOIND_RPCPASS (see docs/native-ubuntu.md)"
		return 1
	fi
	local reply
	reply=$(curl -s -u "$auth" -H 'content-type: text/plain' \
		--data '{"jsonrpc":"1.0","id":"lf","method":"getblockcount","params":[]}' \
		"http://$BITCOIND_RPCHOST/" || true)
	case "$reply" in
	*'"result":'[0-9]*) ok "RPC at $BITCOIND_RPCHOST answers with these credentials" ;;
	*)
		warn "RPC at $BITCOIND_RPCHOST did not accept these credentials (reply: ${reply:-none})"
		return 1
		;;
	esac

	ZMQ_BLOCK='' ZMQ_TX=''
	local zmq
	zmq=$(bcli getzmqnotifications 2>/dev/null || echo '[]')
	ZMQ_BLOCK=$(printf '%s' "$zmq" | tr -d '\n ' | grep -o '"type":"pubrawblock","address":"[^"]*"' | sed 's/.*"address":"//; s/"$//' | head -1)
	ZMQ_TX=$(printf '%s' "$zmq" | tr -d '\n ' | grep -o '"type":"pubrawtx","address":"[^"]*"' | sed 's/.*"address":"//; s/"$//' | head -1)
	# Knots may listen on every interface; lnd connects to it locally.
	ZMQ_BLOCK=${ZMQ_BLOCK/0.0.0.0/127.0.0.1}
	ZMQ_TX=${ZMQ_TX/0.0.0.0/127.0.0.1}
	if [ -z "$ZMQ_BLOCK" ] || [ -z "$ZMQ_TX" ]; then
		warn "Knots does not publish blocks and transactions over ZMQ yet. Add to its bitcoin.conf, then restart Knots:"
		printf '     zmqpubrawblock=tcp://127.0.0.1:28332\n     zmqpubrawtx=tcp://127.0.0.1:28333\n'
		return 1
	fi
	ok "ZMQ: blocks $ZMQ_BLOCK, transactions $ZMQ_TX"
}

port_free() {
	! ss -ltnH "sport = :$1" 2>/dev/null | grep -q .
}

step_configure() {
	check_knots || die "fix the Knots notes above, then run: $0 configure"

	say "lnd.conf"
	local conf=$LND_DIR/lnd.conf
	for p in 9735 10009 8080; do
		port_free "$p" || warn "port $p is in use already; edit listen/rpclisten/restlisten in $conf"
	done
	if [ -f "$conf" ]; then
		warn "$conf exists; leaving it alone. Compare it with what this step would write:"
		write_conf /dev/stdout | sed 's/^/     /'
		return
	fi
	run mkdir -p "$LND_DIR"
	write_conf "$conf"
	run chmod 600 "$conf"
	ok "wrote $conf"
	next_steps
}

write_conf() {
	{
		cat <<EOF
# Lightning Fork, written by scripts/native-ubuntu.sh. See docs/native-ubuntu.md
# and sample-lnd.conf for every option.

[Application Options]
alias=$LND_ALIAS
debuglevel=info
# Peers reach you on 9735 if your router forwards it; otherwise you can
# still open channels to others. See docs/native-ubuntu.md, "Being reachable".
listen=0.0.0.0:9735
rpclisten=127.0.0.1:10009
restlisten=127.0.0.1:8080

[Bitcoin]
bitcoin.mainnet=true
bitcoin.node=bitcoind

[Bitcoind]
bitcoind.rpchost=$BITCOIND_RPCHOST
EOF
		if [ -n "$RPC_COOKIE" ]; then
			echo "bitcoind.rpccookie=$RPC_COOKIE"
		else
			echo "bitcoind.rpcuser=$RPC_USER"
			echo "bitcoind.rpcpass=$RPC_PASS"
		fi
		cat <<EOF
bitcoind.zmqpubrawblock=$ZMQ_BLOCK
bitcoind.zmqpubrawtx=$ZMQ_TX
EOF
	} >"$1"
}

next_steps() {
	local lnddir_flag=
	[ "$LND_DIR" = "$HOME/.lnd" ] || lnddir_flag=" --lnddir=$LND_DIR"
	cat <<EOF

Next (docs/native-ubuntu.md explains each):

  Prefer a web dashboard? Run instead: $0 dashboard
  (it starts lnd and the dashboard for you, and you create the wallet there).

  1. Start lnd in its own terminal (or: $0 service):
       lnd$lnddir_flag
  2. In another terminal, create the wallet. Write the 24 words down on
     paper; they and channel.backup are how you recover funds:
       lncli$lnddir_flag create
  3. Watch it sync with the chain:
       lncli$lnddir_flag getinfo
  4. Connect to a peer and open your first channel (docs/native-ubuntu.md,
     "First peers and channels").
EOF
}

tcp_open() {
	timeout 3 bash -c "</dev/tcp/${1%:*}/${1##*:}" 2>/dev/null
}

# Most nodes on this network are reachable only as .onion addresses, so a
# node without Tor can connect to few of them.
step_tor() {
	say "Tor"
	local conf=$LND_DIR/lnd.conf
	[ -f "$conf" ] || die "no $conf yet; run: $0 configure"
	if grep -q '^tor.active' "$conf"; then
		ok "$conf already has Tor settings; leaving them alone"
		return
	fi
	local socks=${TOR_SOCKS:-127.0.0.1:9050} control=${TOR_CONTROL:-127.0.0.1:9051}
	tcp_open "$socks" || die "no Tor SOCKS proxy at $socks. Install Tor first: sudo apt install tor"
	ok "Tor SOCKS proxy at $socks"
	local onion=no cookie=/run/tor/control.authcookie
	if tcp_open "$control" && [ -r "$cookie" ]; then
		onion=yes
		ok "Tor control port at $control, cookie readable: lnd will run an onion service"
	else
		warn "no usable Tor control port: lnd can reach .onion peers, but won't have an onion address of its own"
		warn "for one, add to /etc/tor/torrc: ControlPort 9051, CookieAuthentication 1, CookieAuthFileGroupReadable 1;"
		warn "then: sudo usermod -aG debian-tor $USER, log in again, sudo systemctl restart tor,"
		warn "and add tor.v3=true and tor.control=$control to the [Tor] section of $conf"
	fi
	printf '+ append a [Tor] section to %s\n' "$conf"
	{
		cat <<EOF

[Tor]
tor.active=true
tor.socks=$socks
# Reach clearnet peers directly, onion peers through Tor.
tor.skip-proxy-for-clearnet-targets=true
EOF
		if [ "$onion" = yes ]; then
			printf 'tor.v3=true\ntor.control=%s\n' "$control"
		fi
	} >>"$conf"
	ok "restart lnd for this to take effect"
}

# lnd's unit. With the dashboard, lnd reads the umbrel-lnd.conf the dashboard
# writes (your lnd.conf plus what its settings manage) and must come back by
# itself: the dashboard stops it to apply a change.
write_lnd_unit() {
	local args="--lnddir=$LND_DIR" restart=on-failure
	if [ -f "$LF_CONF/dashboard.env" ]; then
		args="$args --configfile=$LND_DIR/umbrel-lnd.conf"
		restart=always
	fi
	run mkdir -p "$UNIT_DIR"
	cat >"$UNIT_DIR/lightning-fork.service" <<EOF
[Unit]
Description=Lightning Fork (lnd for the Bitcoin BLAKE2b chain)
After=network-online.target

[Service]
ExecStart=$GOBIN_DIR/lnd $args
Restart=$restart
RestartSec=10
TimeoutStopSec=120

[Install]
WantedBy=default.target
EOF
	ok "wrote $UNIT_DIR/lightning-fork.service"
}

# An lnd started by hand would hold the wallet the service needs.
no_stray_lnd() {
	if pgrep -u "$(id -u)" -x lnd >/dev/null && ! systemctl --user is-active --quiet lightning-fork.service; then
		die "lnd is running outside the service. Stop it first (Ctrl-C in its terminal, or: lncli stop), then run this again."
	fi
}

step_service() {
	say "systemd user service"
	no_stray_lnd
	write_lnd_unit
	run systemctl --user daemon-reload
	run systemctl --user enable lightning-fork.service
	run systemctl --user restart lightning-fork.service
	ok "started; logs: journalctl --user -u lightning-fork -f"
	warn "to keep it running when you are logged out and after a reboot: sudo loginctl enable-linger $USER"
	if [ ! -f "$LF_CONF/dashboard.env" ]; then
		warn "after every start the wallet is locked until you run: lncli unlock"
	fi
}

# A value from lnd.conf (the last one wins, as in lnd).
conf_value() {
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$LND_DIR/lnd.conf" | tail -1
}

# This machine's address on its network: the one it would send from, not a
# container bridge.
lan_ip() {
	ip route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}'
}

random_password() {
	head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 24
}

# A value for a systemd EnvironmentFile, quoted.
env_quote() {
	local v=${1//\\/\\\\}
	printf '"%s"' "${v//\"/\\\"}"
}

step_dashboard() {
	[ -f "$LND_DIR/lnd.conf" ] || die "no $LND_DIR/lnd.conf yet; run: $0 configure"
	[ -x "$GOBIN_DIR/lnd" ] || die "lnd is not built yet; run: $0 build"
	check_knots || die "fix the Knots notes above, then run: $0 dashboard"
	no_stray_lnd

	say "Node.js $NODE_VERSION (for the dashboard)"
	local node_dir=$LF_SHARE/node
	if [ "$("$node_dir/bin/node" -v 2>/dev/null)" = "v$NODE_VERSION" ]; then
		ok "already at $node_dir"
	else
		local arch sum tarball tmp
		case "$(go_arch)" in
		amd64) arch=x64 sum=$NODE_SHA256_x64 ;;
		arm64) arch=arm64 sum=$NODE_SHA256_arm64 ;;
		esac
		tarball="node-v$NODE_VERSION-linux-$arch.tar.xz"
		tmp=$(mktemp -d)
		run curl -fsSL -o "$tmp/$tarball" "https://nodejs.org/dist/v$NODE_VERSION/$tarball"
		printf '+ echo "%s  %s" | sha256sum -c\n' "$sum" "$tarball"
		(cd "$tmp" && echo "$sum  $tarball" | sha256sum -c --quiet) ||
			die "the download does not match nodejs.org's published checksum"
		run rm -rf "$node_dir"
		run mkdir -p "$LF_SHARE"
		run tar -C "$LF_SHARE" -xJf "$tmp/$tarball"
		run mv "$LF_SHARE/node-v$NODE_VERSION-linux-$arch" "$node_dir"
		rm -rf "$tmp"
		ok "$("$node_dir/bin/node" -v)"
	fi
	local npath="$node_dir/bin:$PATH"

	say "Dashboard $DASHBOARD_REF"
	local dash=$LF_SHARE/dashboard
	if [ -d "$dash/.git" ]; then
		run git -C "$dash" fetch -q --tags origin
	else
		run git clone -q "$DASHBOARD_REPO" "$dash"
	fi
	# This copy is the script's own: local edits in it are discarded.
	run git -C "$dash" -c advice.detachedHead=false checkout -q --force "$DASHBOARD_REF"
	(cd "$dash" && run env PATH="$npath" npm ci --no-audit --no-fund --loglevel=error)
	printf '+ (cd %s && npm run build:frontend) > %s/build.log\n' "$dash" "$dash"
	(cd "$dash" && env PATH="$npath" npm run build:frontend >build.log 2>&1) ||
		die "the dashboard did not build; see $dash/build.log"
	ok "built in $dash"

	say "Channel backup helper"
	# The versions and checksums the dashboard's own image uses.
	local bin=$LF_SHARE/bin
	arg() { sed -n "s/^ARG $1=//p" "$dash/Dockerfile" | head -1; }
	local rclone_v ref agent_sum rclone_sum
	rclone_v=$(arg RCLONE_VERSION)
	ref=$(arg BACKUP_AGENT_REF)
	agent_sum=$(arg BACKUP_AGENT_SHA256)
	case "$(go_arch)" in
	amd64) rclone_sum=$(arg RCLONE_SHA256_AMD64) ;;
	arm64) rclone_sum=$(arg RCLONE_SHA256_ARM64) ;;
	esac
	run mkdir -p "$bin"
	if [ "$("$bin/rclone" version 2>/dev/null | head -1)" != "rclone $rclone_v" ]; then
		local tmp
		tmp=$(mktemp -d)
		run curl -fsSL -o "$tmp/rclone.zip" "https://downloads.rclone.org/$rclone_v/rclone-$rclone_v-linux-$(go_arch).zip"
		(cd "$tmp" && echo "$rclone_sum  rclone.zip" | sha256sum -c --quiet) ||
			die "rclone's download does not match the checksum the dashboard pins"
		run unzip -q -j -o "$tmp/rclone.zip" '*/rclone' -d "$bin"
		rm -rf "$tmp"
	fi
	ok "$("$bin/rclone" version | head -1)"
	run curl -fsSL -o "$bin/backup-agent.sh" "https://raw.githubusercontent.com/paulscode/lightning-fork-startos/$ref/backup-agent.sh"
	echo "$agent_sum  $bin/backup-agent.sh" | sha256sum -c --quiet ||
		die "backup-agent.sh does not match the checksum the dashboard pins"
	chmod 755 "$bin/backup-agent.sh"
	ok "backup-agent.sh at $ref"

	say "Dashboard settings"
	local data=$LF_SHARE/dashboard-data
	run mkdir -p "$data" "$LF_CONF"
	chmod 700 "$data" "$LF_CONF"
	local pwfile=$LF_CONF/dashboard-password.json
	if [ ! -f "$pwfile" ]; then
		(umask 077 && printf '{"password": "%s"}\n' "$(random_password)" >"$pwfile")
		ok "made a sign-in password: $pwfile"
	fi
	local env=$LF_CONF/dashboard.env wallet_db=$LND_DIR/data/chain/bitcoin/mainnet/wallet.db
	if [ -f "$env" ]; then
		ok "$env exists; keeping it"
		# The one setting a later run adds: the phone app's port.
		if [ -n "${MOBILE_PORT:-}" ] && ! grep -q '^MOBILE_TLS_PORT=' "$env"; then
			printf 'MOBILE_TLS_PORT=%s\nMOBILE_LAN_IP=%s\nDEVICE_DOMAIN_NAME=%s.local\n' \
				"$MOBILE_PORT" "$(lan_ip)" "$(hostname -s)" >>"$env"
			ok "added the phone app's port, $MOBILE_PORT"
		fi
	else
		# The dashboard unlocks the wallet after every start, with this password.
		local wallet_pw
		if [ -f "$wallet_db" ]; then
			echo "   A wallet exists already. The dashboard unlocks it after every start, so it needs"
			echo "   its password (kept in $env, readable only by you)."
			read -rsp "   Wallet password: " wallet_pw
			echo
			[ -n "$wallet_pw" ] || die "no password given"
			# Its seed was shown when you made it, so the dashboard's first-run
			# steps (create or restore a wallet) don't apply.
			[ -f "$data/state.json" ] || (umask 077 && echo '{"onboarding": false}' >"$data/state.json")
		else
			wallet_pw=$(random_password)
		fi
		local rpchost=${BITCOIND_RPCHOST%:*} rpcport=${BITCOIND_RPCHOST##*:}
		local grpc rest socks
		grpc=$(conf_value rpclisten | sed 's/.*://')
		rest=$(conf_value restlisten | sed 's/.*://')
		socks=$(conf_value tor.socks)
		(
			umask 077
			{
				cat <<EOF
# The dashboard's settings, written by scripts/native-ubuntu.sh.
DASHBOARD_PLATFORM=native
HOST=$DASHBOARD_HOST
PORT=$DASHBOARD_PORT
DASHBOARD_PASSWORD_FILE=$pwfile
LND_WALLET_PASSWORD=$(env_quote "$wallet_pw")
LND_NETWORK=mainnet
LND_HOST=127.0.0.1
LND_PORT=${grpc:-10009}
LND_GRPC_PORT=${grpc:-10009}
LND_REST_PORT=${rest:-8080}
LND_DIR=$LND_DIR
TLS_FILE=$LND_DIR/tls.cert
MACAROON_DIR=$LND_DIR/data/chain/bitcoin/mainnet/
CHANNEL_BACKUP_FILE=$LND_DIR/data/chain/bitcoin/mainnet/channel.backup
LND_CONF_FILEPATH=$LND_DIR/lnd.conf
UMBREL_LND_CONF_FILEPATH=$LND_DIR/umbrel-lnd.conf
LND_INITIALIZE_WITH_TOR_ONLY=unset
JSON_STORE_FILE=$data/state.json
JSON_SETTINGS_FILE=$data/settings.json
USER_FILE=$data/user.json
TERMS_ACKNOWLEDGE_FILE=$data/terms-acknowledge.json
MANAGED_CHANNELS_FILE=$data/managedChannels.json
BACKUP_AGENT=$bin/backup-agent.sh
PATH=$bin:/usr/local/bin:/usr/bin:/bin
BITCOIN_HOST=$rpchost
RPC_PORT=$rpcport
EOF
			if [ -n "$RPC_COOKIE" ]; then
				echo "RPC_COOKIE_FILE=$RPC_COOKIE"
			else
				echo "RPC_USER=$(env_quote "$RPC_USER")"
				echo "RPC_PASSWORD=$(env_quote "$RPC_PASS")"
			fi
			if [ -n "$socks" ]; then
				echo "TOR_PROXY_IP=${socks%:*}"
				echo "TOR_PROXY_PORT=${socks##*:}"
			fi
			# The phone app's own TLS listener, on every interface, as on Umbrel;
			# phones prove themselves with keys made at pairing.
			if [ -n "${MOBILE_PORT:-}" ]; then
				echo "MOBILE_TLS_PORT=$MOBILE_PORT"
				echo "MOBILE_LAN_IP=$(lan_ip)"
				echo "DEVICE_DOMAIN_NAME=$(hostname -s).local"
			fi
			} >"$env"
		)
		ok "wrote $env"
	fi
	# lnd's first start under the dashboard reads this; the dashboard rewrites
	# it at its own start and restarts lnd.
	[ -f "$LND_DIR/umbrel-lnd.conf" ] || run cp "$LND_DIR/lnd.conf" "$LND_DIR/umbrel-lnd.conf"

	say "Services"
	write_lnd_unit
	cat >"$UNIT_DIR/lightning-fork-dashboard.service" <<EOF
[Unit]
Description=Lightning Fork dashboard
After=lightning-fork.service
Wants=lightning-fork.service

[Service]
WorkingDirectory=$dash/apps/backend
EnvironmentFile=$env
ExecStart=$node_dir/bin/node ./bin/www
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF
	ok "wrote $UNIT_DIR/lightning-fork-dashboard.service"
	run systemctl --user daemon-reload
	run systemctl --user enable lightning-fork.service lightning-fork-dashboard.service
	run systemctl --user restart lightning-fork.service lightning-fork-dashboard.service

	local url="http://127.0.0.1:$DASHBOARD_PORT"
	if [ "$DASHBOARD_HOST" != 127.0.0.1 ] && [ "$DASHBOARD_HOST" != localhost ]; then
		url="$url  (from other devices: http://$(lan_ip):$DASHBOARD_PORT)"
	fi
	cat <<EOF

The dashboard is starting (give it half a minute):

  Open:      $url
  Password:  $(sed -n 's/.*"password": *"\([^"]*\)".*/\1/p' "$pwfile")   (in $pwfile; change it there)

EOF
	if [ -f "$wallet_db" ]; then
		echo "It unlocks your wallet itself after every start; no more lncli unlock."
	else
		echo "Create your wallet there: it shows 24 words; write them down on paper."
	fi
	if grep -q '^MOBILE_TLS_PORT=' "$env"; then
		echo "Phone app: pair from the dashboard's Mobile app menu, on this network (port $(sed -n 's/^MOBILE_TLS_PORT=//p' "$env"))."
	fi
	cat <<EOF
Logs:      journalctl --user -u lightning-fork-dashboard -f
           journalctl --user -u lightning-fork -f
After editing $LND_DIR/lnd.conf: systemctl --user restart lightning-fork-dashboard
EOF
	warn "to keep both running when you are logged out and after a reboot: sudo loginctl enable-linger $USER"
}

case "${1:-}" in
check) step_check ;;
deps) step_deps ;;
go) step_go ;;
build) step_build ;;
configure) step_configure ;;
tor) step_tor ;;
service) step_service ;;
dashboard) step_dashboard ;;
all)
	step_deps
	step_go
	step_build
	step_configure
	;;
*)
	sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
