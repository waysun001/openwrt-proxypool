#!/bin/sh
set -eu

CURL="${ZEANLINK_CURL:-/usr/bin/curl}"
DATE="${ZEANLINK_DATE:-/bin/date}"
LOGGER="${ZEANLINK_LOGGER:-/usr/bin/logger}"

[ -x "$CURL" ] && [ -x "$DATE" ] || exit 1

current_year=$($DATE -u +%Y 2>/dev/null || printf '0')
case "$current_year" in
	''|*[!0-9]*) current_year=0 ;;
esac
[ "$current_year" -lt 2025 ] || exit 0

headers=$(mktemp /tmp/zeanlink-time.XXXXXX) || exit 1
trap 'rm -f "$headers"' EXIT HUP INT TERM

normalize_http_date() {
	date_value=$1
	set -- $date_value
	[ "$#" -eq 6 ] && [ "$6" = GMT ] || return 1
	case "$1" in
		Mon,|Tue,|Wed,|Thu,|Fri,|Sat,|Sun,) ;;
		*) return 1 ;;
	esac
	case "$2" in
		0[1-9]|[12][0-9]|3[01]) ;;
		*) return 1 ;;
	esac
	case "$3" in
		Jan) month=01 ;; Feb) month=02 ;; Mar) month=03 ;; Apr) month=04 ;;
		May) month=05 ;; Jun) month=06 ;; Jul) month=07 ;; Aug) month=08 ;;
		Sep) month=09 ;; Oct) month=10 ;; Nov) month=11 ;; Dec) month=12 ;;
		*) return 1 ;;
	esac
	case "$4" in
		202[5-9]|20[3-9][0-9]|2100) ;;
		*) return 1 ;;
	esac
	case "$5" in
		[01][0-9]:[0-5][0-9]:[0-5][0-9]|2[0-3]:[0-5][0-9]:[0-5][0-9]) ;;
		*) return 1 ;;
	esac
	printf '%s-%s-%s %s\n' "$4" "$month" "$2" "$5"
}

fetch_time() {
	url=$1
	: >"$headers"
	"$CURL" --noproxy '*' --silent --show-error --connect-timeout 3 --max-time 5 \
		--max-redirs 0 --proto '=http' --dump-header "$headers" --output /dev/null "$url" || return 1
	date_lines=$(sed -n 's/^[Dd][Aa][Tt][Ee]:[[:space:]]*//p' "$headers" | tr -d '\r')
	[ "$(printf '%s\n' "$date_lines" | sed '/^$/d' | wc -l)" -eq 1 ] || return 1
	normalized=$(normalize_http_date "$date_lines") || return 1
	epoch=$($DATE -u -d "$normalized" +%s 2>/dev/null) || return 1
	case "$epoch" in
		''|*[!0-9]*) return 1 ;;
	esac
	printf '%s|%s\n' "$normalized" "$epoch"
}

cloudflare=$(fetch_time http://1.1.1.1/ || fetch_time http://1.0.0.1/) || exit 1
opendns=$(fetch_time http://208.67.222.222/ || fetch_time http://208.67.220.220/) || exit 1
cloudflare_epoch=${cloudflare##*|}
opendns_epoch=${opendns##*|}
if [ "$cloudflare_epoch" -ge "$opendns_epoch" ]; then
	difference=$((cloudflare_epoch - opendns_epoch))
else
	difference=$((opendns_epoch - cloudflare_epoch))
fi
[ "$difference" -le 300 ] || exit 1

trusted_time=${cloudflare%|*}
"$DATE" -u -s "$trusted_time" >/dev/null
if [ -x "$LOGGER" ]; then
	"$LOGGER" -t zeanlink-time 'WAN time restored from two numeric HTTP authorities'
fi
