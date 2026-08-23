#!/usr/bin/env bash
# Coverage ratchet gate.
#
# Statement coverage may never drop below the recorded baseline in
# scripts/coverage-baseline. It may rise: when it does, the script suggests
# moving the gate up, and `--update` does so automatically. This keeps CI
# honest today (100% totals are unreachable while provider adapters and
# example mains ship untested paths) while making regressions impossible to
# merge.
#
# Record baselines in a plain `go test ./...` environment: optional contract
# suites gated behind env vars such as LEBRO_POSTGRES_TEST_DSN only ever add
# covered statements, so a baseline recorded without them cannot false-fail a
# richer environment.
set -euo pipefail

cd "$(dirname "$0")/.."

baseline_file="scripts/coverage-baseline"
profile="${COVERAGE_PROFILE:-coverage.out}"
update=false
if [[ "${1:-}" == "--update" ]]; then
	update=true
fi

if [[ ! -f "$baseline_file" ]]; then
	echo "coverage baseline file $baseline_file is missing" >&2
	echo 'create it with the minimum allowed total, e.g.: printf '"'"'%s\n'"'"' '"'"'68.9%'"'"' > '"$baseline_file" >&2
	exit 1
fi
baseline="$(tr -d '[:space:]' < "$baseline_file")"
if [[ -z "$baseline" ]]; then
	echo "coverage baseline file $baseline_file is empty" >&2
	exit 1
fi

go test ./... -coverprofile="$profile"

total="$(go tool cover -func="$profile" | awk '/^total:/{print $3}')"

# Compare as numbers so "68.90%" and "68.9%" agree.
lower() { awk -v candidate="$1" -v floor="$2" 'BEGIN { exit !(candidate + 0 < floor + 0) }'; }

if lower "${total%\%}" "${baseline%\%}"; then
	echo "statement coverage: ${total}; required at least ${baseline} (ratchet)" >&2
	echo "fix the uncovered regression, or intentionally move the gate:" >&2
	echo "  printf '%s\\n' '${total}' > $baseline_file" >&2
	exit 1
fi

if [[ "$total" != "$baseline" ]]; then
	if [[ "$update" == true ]]; then
		printf '%s\n' "$total" > "$baseline_file"
		echo "statement coverage: ${total}; baseline moved to $(cat "$baseline_file")"
	else
		echo "statement coverage: ${total}; baseline ${baseline}. Consider raising the gate:"
		echo "  bash scripts/check-coverage.sh --update"
	fi
else
	echo "statement coverage: ${total}"
fi
