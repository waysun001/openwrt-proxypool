#!/bin/sh
# Phase 1 quarantine admission for every legacy runtime mutation.
#
# Accepted forms are:
#   legacy-gate.sh mutation <scope>
#   legacy-gate.sh <scope>
# Admission is deliberately independent of scope and machine state.

case "${1:-}" in
	mutation)
		mutation_scope="${2:-unspecified}"
		;;
	*)
		mutation_scope="${1:-unspecified}"
		;;
esac

: "$mutation_scope"
printf '%s\n' 'legacy_runtime_quarantined'
exit 125
