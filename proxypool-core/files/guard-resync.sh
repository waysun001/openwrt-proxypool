#!/bin/sh
set -eu

NFT="${PROXYPOOL_NFT:-/usr/sbin/nft}"
RULESET="${PROXYPOOL_GUARD_RULESET:-/usr/lib/proxypool/proxypool-guard.nft}"
TRANSACTION_HELPER="${PROXYPOOL_TRANSACTION_HELPER:-/usr/lib/proxypool/proxypool-firewall-transaction}"
COLD_FINALIZER="${PROXYPOOL_COLD_FINALIZER:-}"

[ "$#" -le 1 ] || {
	echo 'ProxyPool guardian: expected at most one mode argument' >&2
	exit 1
}
MODE=${1:-auto}
case "$MODE" in
	auto|reset-empty) : ;;
	*)
		echo "ProxyPool guardian: unsupported mode: $MODE" >&2
		exit 1
	;;
esac

[ -x "$NFT" ] || {
	echo "ProxyPool guardian: nft is not executable: $NFT" >&2
	exit 1
}
[ -r "$RULESET" ] || {
	echo "ProxyPool guardian: ruleset is not readable: $RULESET" >&2
	exit 1
}

# Phase 1 has no trusted ownership manifest publisher yet.  Rebuild the
# permanent guardian with empty authorization sets and deliberately publish
# nothing.  Future resync may add tuples only after validating a root-owned
# generation manifest and the corresponding exact ready state.
"$NFT" -f "$RULESET"

# fw4 executes compatible script includes only after its atomic nft apply and
# exports ACTION=includes.  Include failures are warning-only upstream, so the
# WAL is retired solely after guardian reset and an explicit finalizer success.
if [ "$MODE" = auto ] && [ "${ACTION:-}" = includes ]; then
	if [ -n "$COLD_FINALIZER" ]; then
		[ -x "$COLD_FINALIZER" ] || {
			echo "ProxyPool guardian: finalizer is not executable: $COLD_FINALIZER" >&2
			exit 1
		}
		"$COLD_FINALIZER"
	else
		[ -x "$TRANSACTION_HELPER" ] || {
			echo "ProxyPool guardian: transaction helper is not executable: $TRANSACTION_HELPER" >&2
			exit 1
		}
		"$TRANSACTION_HELPER" finalize-fw4-locked
	fi
fi
