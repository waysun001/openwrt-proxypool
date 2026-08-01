#!/bin/sh
set -eu

umask 077

LEGACY_CONFIG="${PROXYPOOL_BACKUP_LEGACY_CONFIG:-/etc/config/proxypool}"
V2_CONFIG="${PROXYPOOL_BACKUP_V2_CONFIG:-/etc/config/proxypool_v2}"
SELECTOR_CONFIG="${PROXYPOOL_BACKUP_SELECTOR_CONFIG:-/etc/config/proxypool_runtime}"
TMP_ROOT="${PROXYPOOL_BACKUP_TMPDIR:-/tmp}"
TAR="${PROXYPOOL_BACKUP_TAR:-tar}"
SHA256SUM="${PROXYPOOL_BACKUP_SHA256SUM:-sha256sum}"
SYNC="${PROXYPOOL_BACKUP_SYNC:-sync}"
MKTEMP="${PROXYPOOL_BACKUP_MKTEMP:-mktemp}"
WORK_DIR=
OUTPUT_TEMP=

fail() {
	echo "ProxyPool backup: $*" >&2
	exit 1
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	if [ -n "$OUTPUT_TEMP" ] && [ -f "$OUTPUT_TEMP" ] && [ ! -L "$OUTPUT_TEMP" ]; then
		rm -f -- "$OUTPUT_TEMP" || status=1
	fi
	if [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ] && [ ! -L "$WORK_DIR" ]; then
		rm -rf -- "$WORK_DIR" || status=1
	fi
	exit "$status"
}

case "${1:-}" in
	restore)
		# Phase 1 intentionally has no transactional full-dual restore.  Reject
		# before inspecting the input, creating a directory, stopping a service,
		# extracting an archive, or writing any configuration byte.
		echo 'restore_unsupported: transactional dual-config restore is not implemented' >&2
		exit 2
		;;
	create|verify) ACTION=$1 ;;
	*) fail 'usage: backup.sh {create|verify|restore} FILE' ;;
esac
[ "$#" -eq 2 ] || fail "$ACTION expects exactly one file argument"
ARCHIVE=$2

for command_name in "$TAR" "$SHA256SUM" "$SYNC" "$MKTEMP" awk chmod cp dirname grep ln mkdir rm sed sort tr wc; do
	command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

require_absolute_path() {
	path=$1
	description=$2
	case "$path" in
		/*) : ;;
		*) fail "$description must be absolute" ;;
	esac
	[ "$path" != / ] || fail "$description must not be the filesystem root"
	printf '%s\n' "$path" | grep -Eq '^/[A-Za-z0-9._/-]+$' ||
		fail "$description contains unsupported characters"
	case "$path" in
		*/../*|*/..|*/./*|*/.|*//*) fail "$description is not canonical" ;;
	esac
}

require_safe_parent() {
	path=$1
	description=$2
	parent=$(dirname "$path")
	name=${path##*/}
	[ -n "$name" ] && [ "$name" != . ] && [ "$name" != .. ] ||
		fail "$description has an unsafe basename"
	[ -d "$parent" ] && [ ! -L "$parent" ] || fail "$description parent is unsafe"
	resolved_parent=$(CDPATH= cd -- "$parent" && pwd -P) || fail "cannot resolve $description parent"
	[ "$path" = "$resolved_parent/$name" ] || fail "$description parent is not canonical"
}

require_absolute_path "$TMP_ROOT" 'temporary directory'
[ -d "$TMP_ROOT" ] && [ ! -L "$TMP_ROOT" ] || fail 'temporary directory is unsafe'
resolved_tmp=$(CDPATH= cd -- "$TMP_ROOT" && pwd -P) || fail 'cannot resolve temporary directory'
[ "$TMP_ROOT" = "$resolved_tmp" ] || fail 'temporary directory is not canonical'

WORK_DIR=$("$MKTEMP" -d "$TMP_ROOT/proxypool-backup.XXXXXX") ||
	fail 'cannot create private backup workspace'
case "$WORK_DIR" in
	"$TMP_ROOT"/proxypool-backup.*) : ;;
	*) WORK_DIR=; fail 'temporary workspace escaped its parent' ;;
esac
[ -d "$WORK_DIR" ] && [ ! -L "$WORK_DIR" ] || fail 'temporary workspace is unsafe'
chmod 700 "$WORK_DIR" || fail 'cannot protect backup workspace'
trap cleanup EXIT HUP INT TERM

path_state() {
	if [ -e "$1" ] || [ -L "$1" ]; then
		[ -f "$1" ] && [ ! -L "$1" ] && [ -s "$1" ] || return 2
		return 0
	fi
	return 1
}

hash_file() {
	output=$("$SHA256SUM" "$1") || fail "cannot hash $1"
	digest=${output%%[[:space:]]*}
	[ "${#digest}" -eq 64 ] && printf '%s\n' "$digest" | grep -Eq '^[0-9A-Fa-f]{64}$' ||
		fail "invalid SHA-256 output for $1"
	printf '%s\n' "$digest"
}

classify_sources() {
	legacy=0 v2=0 selector=0
	if path_state "$LEGACY_CONFIG"; then legacy=1; else result=$?; [ "$result" -eq 1 ] || fail 'unsafe V1 config'; fi
	if path_state "$V2_CONFIG"; then v2=1; else result=$?; [ "$result" -eq 1 ] || fail 'unsafe V2 config'; fi
	if path_state "$SELECTOR_CONFIG"; then selector=1; else result=$?; [ "$result" -eq 1 ] || fail 'unsafe selector config'; fi
	if [ "$legacy" -eq 1 ] && [ "$v2" -eq 0 ] && [ "$selector" -eq 0 ]; then
		KIND=legacy-only
	elif [ "$legacy" -eq 1 ] && [ "$v2" -eq 1 ] && [ "$selector" -eq 1 ]; then
		KIND=full-dual
	else
		fail 'partial-dual configuration cannot be backed up'
	fi
}

create_backup() {
	require_absolute_path "$ARCHIVE" 'backup destination'
	require_safe_parent "$ARCHIVE" 'backup destination'
	[ ! -e "$ARCHIVE" ] && [ ! -L "$ARCHIVE" ] || fail 'backup destination already exists'
	classify_sources

	mkdir "$WORK_DIR/config" || fail 'cannot create staged config directory'
	cp "$LEGACY_CONFIG" "$WORK_DIR/config/proxypool" || fail 'cannot stage V1 config'
	chmod 600 "$WORK_DIR/config/proxypool" || fail 'cannot protect staged V1 config'
	legacy_hash=$(hash_file "$WORK_DIR/config/proxypool")
	if [ "$KIND" = full-dual ]; then
		cp "$V2_CONFIG" "$WORK_DIR/config/proxypool_v2" || fail 'cannot stage V2 config'
		cp "$SELECTOR_CONFIG" "$WORK_DIR/config/proxypool_runtime" || fail 'cannot stage selector config'
		chmod 600 "$WORK_DIR/config/proxypool_v2" "$WORK_DIR/config/proxypool_runtime" ||
			fail 'cannot protect staged dual config'
		v2_hash=$(hash_file "$WORK_DIR/config/proxypool_v2")
		selector_hash=$(hash_file "$WORK_DIR/config/proxypool_runtime")
	fi
	{
		printf 'schema_version=1\n'
		printf 'kind=%s\n' "$KIND"
		printf 'proxypool_sha256=%s\n' "$legacy_hash"
		if [ "$KIND" = full-dual ]; then
			printf 'proxypool_v2_sha256=%s\n' "$v2_hash"
			printf 'proxypool_runtime_sha256=%s\n' "$selector_hash"
		fi
	} >"$WORK_DIR/manifest" || fail 'cannot write backup manifest'
	chmod 600 "$WORK_DIR/manifest" || fail 'cannot protect backup manifest'

	archive_parent=$(dirname "$ARCHIVE")
	OUTPUT_TEMP=$("$MKTEMP" "$archive_parent/.proxypool-backup.XXXXXX") ||
		fail 'cannot create temporary backup archive'
	[ -f "$OUTPUT_TEMP" ] && [ ! -L "$OUTPUT_TEMP" ] || fail 'temporary backup archive is unsafe'
	"$TAR" -czf "$OUTPUT_TEMP" -C "$WORK_DIR" manifest config || fail 'cannot create backup archive'
	chmod 600 "$OUTPUT_TEMP" || fail 'cannot protect backup archive'
	"$SYNC" || fail 'cannot sync backup archive'
	# A same-directory hard link publishes without overwriting a path that may
	# have appeared after the preflight check.
	ln "$OUTPUT_TEMP" "$ARCHIVE" || fail 'cannot atomically publish backup archive'
	"$SYNC" || fail 'cannot sync published backup archive'
	rm -f "$OUTPUT_TEMP" || fail 'cannot retire temporary backup archive link'
	OUTPUT_TEMP=
	printf '%s\n' "$ARCHIVE"
}

read_manifest_digest() {
	key=$1
	line_number=$2
	line=$(sed -n "${line_number}p" "$WORK_DIR/manifest.read") || return 1
	case "$line" in
		"$key="*) digest=${line#*=} ;;
		*) return 1 ;;
	esac
	[ "${#digest}" -eq 64 ] && printf '%s\n' "$digest" | grep -Eq '^[0-9A-Fa-f]{64}$' || return 1
	printf '%s\n' "$digest"
}

verify_member_hash() {
	member=$1
	expected=$2
	bytes=$("$TAR" -xOzf "$ARCHIVE" "$member" 2>/dev/null | wc -c | tr -d '[:space:]') || return 1
	[ "$bytes" -gt 0 ] 2>/dev/null || return 1
	output=$("$TAR" -xOzf "$ARCHIVE" "$member" 2>/dev/null | "$SHA256SUM") || return 1
	actual=${output%%[[:space:]]*}
	[ "$actual" = "$expected" ]
}

verify_backup() {
	require_absolute_path "$ARCHIVE" 'backup input'
	require_safe_parent "$ARCHIVE" 'backup input'
	[ -f "$ARCHIVE" ] && [ ! -L "$ARCHIVE" ] && [ -r "$ARCHIVE" ] || fail 'backup input is unsafe'

	listing=$("$TAR" -tzf "$ARCHIVE" 2>/dev/null) || fail 'invalid backup archive'
	verbose=$("$TAR" -tvzf "$ARCHIVE" 2>/dev/null) || fail 'cannot inspect backup archive types'
	if printf '%s\n' "$verbose" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" { bad=1 } END { exit bad }'; then
		:
	else
		fail 'backup archive contains links or special files'
	fi
	[ "$(printf '%s\n' "$listing" | grep -Fxc manifest || true)" -eq 1 ] ||
		fail 'backup archive has no unique manifest'
	"$TAR" -xOzf "$ARCHIVE" manifest >"$WORK_DIR/manifest.read" 2>/dev/null ||
		fail 'cannot read backup manifest'
	[ -f "$WORK_DIR/manifest.read" ] && [ ! -L "$WORK_DIR/manifest.read" ] || fail 'unsafe manifest output'
	[ "$(sed -n '1p' "$WORK_DIR/manifest.read")" = schema_version=1 ] || fail 'unsupported backup schema'
	kind_line=$(sed -n '2p' "$WORK_DIR/manifest.read") || fail 'missing backup kind'
	case "$kind_line" in
		kind=legacy-only) KIND=legacy-only; expected_lines=3 ;;
		kind=full-dual) KIND=full-dual; expected_lines=5 ;;
		*) fail 'invalid backup kind' ;;
	esac
	[ "$(wc -l <"$WORK_DIR/manifest.read" | tr -d '[:space:]')" -eq "$expected_lines" ] ||
		fail 'backup manifest has the wrong line count'

	legacy_hash=$(read_manifest_digest proxypool_sha256 3) || fail 'invalid V1 config digest'
	if [ "$KIND" = legacy-only ]; then
		expected_listing=$(printf '%s\n' manifest config/ config/proxypool | sort)
	else
		v2_hash=$(read_manifest_digest proxypool_v2_sha256 4) || fail 'invalid V2 config digest'
		selector_hash=$(read_manifest_digest proxypool_runtime_sha256 5) || fail 'invalid selector digest'
		expected_listing=$(printf '%s\n' manifest config/ config/proxypool config/proxypool_v2 config/proxypool_runtime | sort)
	fi
	actual_listing=$(printf '%s\n' "$listing" | sort)
	[ "$actual_listing" = "$expected_listing" ] || fail 'backup archive member set is not canonical'
	verify_member_hash config/proxypool "$legacy_hash" || fail 'V1 config integrity check failed'
	if [ "$KIND" = full-dual ]; then
		verify_member_hash config/proxypool_v2 "$v2_hash" || fail 'V2 config integrity check failed'
		verify_member_hash config/proxypool_runtime "$selector_hash" || fail 'selector integrity check failed'
	fi
	printf '{"valid":true,"schema_version":1,"kind":"%s"}\n' "$KIND"
}

case "$ACTION" in
	create) create_backup ;;
	verify) verify_backup ;;
esac
