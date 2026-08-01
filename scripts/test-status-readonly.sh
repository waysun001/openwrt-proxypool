#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
STATUS_SCRIPT="$ROOT/proxypool-core/files/status.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

fail() {
	printf 'ProxyPool read-only status contract: %s\n' "$*" >&2
	exit 1
}

BIN="$TEST_TMP/bin"
RUN_DIR="$TEST_TMP/run"
LOG_FILE="$TEST_TMP/proxypool.log"
MUTATOR_TRACE="$TEST_TMP/mutators.trace"
LAUNCH_TRACE="$TEST_TMP/launch.trace"
PROBE_TRACE="$TEST_TMP/probe.trace"
IPLOCATION_TRACE="$TEST_TMP/iplocation.trace"
OUTPUT_GET="$TEST_TMP/get.json"
OUTPUT_CLIENT="$TEST_TMP/client.json"
OUTPUT_DEVICES="$TEST_TMP/devices.json"

mkdir -p \
	"$BIN" \
	"$RUN_DIR/location_cache" \
	"$RUN_DIR/counters" \
	"$RUN_DIR/timeout" \
	"$RUN_DIR/probe" \
	"$RUN_DIR/redsocks" \
	"$RUN_DIR/slp/slp-live"
printf '%s\n' 'Existing cache place' >"$RUN_DIR/location_cache/cached.example.txt"
printf '%s\n' 'counter sentinel' >"$RUN_DIR/counters/sentinel"
printf '%s\n' 'timeout sentinel' >"$RUN_DIR/timeout/sentinel"
printf '%s\n' 'ok' >"$RUN_DIR/probe/live"
printf '%s\n' 'ok' >"$RUN_DIR/probe/socks-live"
printf '%s\n' 'ok' >"$RUN_DIR/probe/slp-live"
printf '%s\n' "$$" >"$RUN_DIR/redsocks/socks-live.pid"
printf '%s\n' "$$" >"$RUN_DIR/redsocks/slp-live.pid"
printf '%s\n' "$$" >"$RUN_DIR/slp/slp-live/slp.pid"
printf '%s\n' 'log sentinel: do not append' >"$LOG_FILE"
: >"$MUTATOR_TRACE"
: >"$LAUNCH_TRACE"
: >"$PROBE_TRACE"
: >"$IPLOCATION_TRACE"

cat >"$BIN/uci" <<'EOF_UCI'
#!/usr/bin/env sh
if [ "${1:-}" = -q ]; then
	shift
fi

case "${1:-}" in
	show)
		printf '%s\n' \
			'proxypool.cached=client' \
			'proxypool.miss=client' \
			'proxypool.live=client' \
			'proxypool.socks-live=client' \
			'proxypool.slp-live=client'
		;;
	get)
		case "${2:-}" in
			proxypool.global.enabled) printf '%s\n' 'not-a-json-number' ;;
			proxypool.cached.type) printf '%s\n' 'socks5' ;;
			proxypool.cached.name) printf 'Cached "\001client"\n' ;;
			proxypool.cached.server) printf '%s\n' 'cached.example' ;;
			proxypool.cached.enabled) printf '%s\n' '0' ;;
			proxypool.miss.type) printf '%s\n' 'odd"type' ;;
			proxypool.miss.name) printf '%s\n' 'Missing cache client' ;;
			proxypool.miss.server) printf '%s\n' 'missing.example' ;;
			proxypool.miss.enabled) printf '%s\n' 'not-a-json-number' ;;
			proxypool.miss.bind_ip) printf '%s\n' '192.168.9.23 bad"value\path' ;;
			proxypool.live.type) printf '%s\n' 'l2tp' ;;
			proxypool.live.name) printf '%s\n' 'Live L2TP client' ;;
			proxypool.live.server) printf '%s\n' '198.51.100.7' ;;
			proxypool.live.enabled) printf '%s\n' '1' ;;
			proxypool.socks-live.type) printf '%s\n' 'socks5' ;;
			proxypool.socks-live.name) printf '%s\n' 'Live SOCKS5 client' ;;
			proxypool.socks-live.server) printf '%s\n' '198.51.100.8' ;;
			proxypool.socks-live.enabled) printf '%s\n' '1' ;;
			proxypool.slp-live.type) printf '%s\n' 'slp' ;;
			proxypool.slp-live.name) printf '%s\n' 'Live SLP client' ;;
			proxypool.slp-live.server) printf '%s\n' '198.51.100.9' ;;
			proxypool.slp-live.enabled) printf '%s\n' '1' ;;
			*)
				printf 'controlled missing UCI value: %s\n' "${2:-}" >&2
				exit 1
				;;
		esac
		;;
	*)
		printf 'unsupported controlled UCI call: %s\n' "$*" >&2
		exit 1
		;;
esac
EOF_UCI

cat >"$BIN/nft" <<'EOF_NFT'
#!/usr/bin/env sh
printf '%s\n' 'controlled nft read failure' >&2
exit 1
EOF_NFT

cat >"$BIN/ip" <<'EOF_IP'
#!/usr/bin/env sh
case "$*" in
	'link show ppp-live') exit 0 ;;
	'-4 addr show ppp-live') printf '%s\n' '9: ppp-live    inet 10.77.0.2 peer 10.77.0.1/32'; exit 0 ;;
esac
printf '%s\n' 'controlled ip read failure' >&2
exit 1
EOF_IP

cat >"$BIN/iplocation" <<'EOF_IPLOCATION'
#!/usr/bin/env sh
printf '%s\n' "$1" >>"$PROXYPOOL_TEST_IPLOCATION_TRACE"
printf '%s\n' 'Fresh "read-only" place'
EOF_IPLOCATION

cat >"$BIN/probe" <<'EOF_PROBE'
#!/usr/bin/env sh
printf '%s\n' "$*" >>"$PROXYPOOL_TEST_PROBE_TRACE"
exit 0
EOF_PROBE

cat >"$BIN/nohup" <<'EOF_NOHUP'
#!/usr/bin/env sh
printf 'nohup:%s\n' "$*" >>"$PROXYPOOL_TEST_LAUNCH_TRACE"
exit 0
EOF_NOHUP

for mutator in mkdir touch rm mv cp install tee truncate chmod chown ln; do
	cat >"$BIN/$mutator" <<'EOF_MUTATOR'
#!/usr/bin/env sh
printf '%s:%s\n' "$(basename "$0")" "$*" >>"$PROXYPOOL_TEST_MUTATOR_TRACE"
exit 97
EOF_MUTATOR
done
chmod 755 "$BIN"/*

snapshot_tree() {
	root=$1
	(
		cd "$root"
		find . -type d -print | LC_ALL=C sort
		find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do
			printf 'FILE %s\n' "$file"
			sha256sum "$file"
		done
	)
}

RUNTIME_BEFORE="$TEST_TMP/runtime.before"
RUNTIME_AFTER="$TEST_TMP/runtime.after"
snapshot_tree "$RUN_DIR" >"$RUNTIME_BEFORE"
LOG_BEFORE=$(sha256sum "$LOG_FILE")

run_status() {
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_STATUS_RUN_DIR="$RUN_DIR" \
		PROXYPOOL_STATUS_LOG_FILE="$LOG_FILE" \
		PROXYPOOL_STATUS_IPLOCATION_COMMAND="$BIN/iplocation" \
		PROXYPOOL_STATUS_PROBE_COMMAND="$BIN/probe" \
		PROXYPOOL_TEST_MUTATOR_TRACE="$MUTATOR_TRACE" \
		PROXYPOOL_TEST_LAUNCH_TRACE="$LAUNCH_TRACE" \
		PROXYPOOL_TEST_PROBE_TRACE="$PROBE_TRACE" \
		PROXYPOOL_TEST_IPLOCATION_TRACE="$IPLOCATION_TRACE" \
		sh "$STATUS_SCRIPT" "$@"
}

run_status get >"$OUTPUT_GET" || fail 'get returned nonzero'
run_status client miss >"$OUTPUT_CLIENT" || fail 'client returned nonzero'
run_status devices >"$OUTPUT_DEVICES" || fail 'devices returned nonzero'

# Give an accidentally detached launcher a bounded opportunity to record itself.
sleep 0.2

node -e '
const fs = require("fs");
const get = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const client = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const devices = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
if (!Array.isArray(get.clients) || get.clients.length !== 5) throw new Error("get did not return all clients");
if (get.global_enabled !== 0) throw new Error("invalid global enabled value was not normalized closed");
if (get.clients[0].location !== "Existing cache place") throw new Error("existing location cache was not read");
if (get.clients[0].name !== "Cached \"client\"") throw new Error("JSON control characters were not removed");
if (get.clients[1].location !== "Fresh \"read-only\" place") throw new Error("cache miss did not use the read-only lookup result");
if (get.clients[1].enabled !== 0) throw new Error("invalid client enabled value was not normalized closed");
if (get.clients[2].status !== "connected" || get.clients[2].ip_addr !== "10.77.0.2") {
    throw new Error("status did not derive L2TP state through the read-only runtime path");
}
if (get.clients[3].status !== "connected") throw new Error("status did not derive SOCKS5 state through the read-only runtime path");
if (get.clients[4].status !== "connected") throw new Error("status did not derive SLP state through the read-only runtime path");
if (client.location !== "Fresh \"read-only\" place") throw new Error("client action omitted the read-only lookup result");
if (!Array.isArray(devices)) throw new Error("devices did not return a JSON array");
' "$OUTPUT_GET" "$OUTPUT_CLIENT" "$OUTPUT_DEVICES" || fail 'a read action emitted invalid or unsafe JSON'

snapshot_tree "$RUN_DIR" >"$RUNTIME_AFTER"
cmp -s "$RUNTIME_BEFORE" "$RUNTIME_AFTER" || {
	diff -u "$RUNTIME_BEFORE" "$RUNTIME_AFTER" >&2 || true
	fail 'a status read mutated the runtime tree'
}
[ "$LOG_BEFORE" = "$(sha256sum "$LOG_FILE")" ] || fail 'a status read appended to the log'
[ ! -s "$MUTATOR_TRACE" ] || fail "a status read invoked a mutator: $(tr '\n' ' ' <"$MUTATOR_TRACE")"
[ ! -s "$LAUNCH_TRACE" ] || fail "get attempted to launch a process: $(tr '\n' ' ' <"$LAUNCH_TRACE")"
[ ! -s "$PROBE_TRACE" ] || fail "get executed probe_all: $(tr '\n' ' ' <"$PROBE_TRACE")"
[ ! -e "$RUN_DIR/location_cache/missing.example.txt" ] || fail 'cache miss was persisted'
[ "$(cat "$IPLOCATION_TRACE")" = "missing.example
198.51.100.7
198.51.100.8
198.51.100.9
missing.example" ] || fail 'cache miss did not use the expected read-only lookup path'

printf '%s\n' 'ProxyPool read-only status contract: PASS'
