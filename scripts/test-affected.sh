#!/usr/bin/env bash
# Runs only the Go tests affected by the diff against a base revision.
#
# Usage: scripts/test-affected.sh [base-rev] [-- extra go test args]
#   base-rev defaults to the merge-base with origin/main.
#   TEST_ALL=1 forces the full suite.
#
# agent-sm is a single Go package, so selection is by file area, not package:
#   - foo.go / foo_bar.go  -> tests in foo_test.go, foo_*_test.go (area = name before first "_")
#   - foo_test.go          -> that file's tests
#   - areas without their own tests, shared files (main.go, helpers.go, config.go),
#     go.mod/go.sum, embedded assets (prompts/, web/, examples/) and CI config -> full suite
#   - anything else (docs, .claude/, runs/) -> no tests
set -euo pipefail
cd "$(dirname "$0")/.."

base="${1:-}"
[ "$#" -gt 0 ] && shift
[ "${1:-}" = "--" ] && shift
extra=("$@")

run_all() {
	echo "test-affected: running full suite ($1)"
	exec go test ${extra[@]+"${extra[@]}"} ./...
}

[ "${TEST_ALL:-}" = "1" ] && run_all "TEST_ALL=1"

if [ -z "$base" ]; then
	base=$(git merge-base HEAD origin/main 2>/dev/null) || run_all "no base revision"
fi

changed=$(git diff --name-only "$base" HEAD --; git diff --name-only HEAD --; git ls-files --others --exclude-standard)
changed=$(printf '%s\n' "$changed" | sort -u | sed '/^$/d')

test_files=""
add_test_file() { test_files=$(printf '%s\n%s' "$test_files" "$1"); }
for f in $changed; do
	case "$f" in
	go.mod | go.sum | prompts/* | web/* | examples/* | .github/workflows/* | scripts/test-affected.sh)
		run_all "$f changed" ;;
	main.go | helpers.go | config.go)
		run_all "shared file $f changed" ;;
	*/*.go)
		run_all "$f is outside the root package" ;;
	*_test.go)
		if [ -f "$f" ]; then add_test_file "$f"; fi ;;
	*.go)
		stem=${f%.go}
		area=${stem%%_*}
		matches=$(ls "$area"_test.go "$area"_*_test.go 2>/dev/null || true)
		[ -z "$matches" ] && run_all "$f has no area tests"
		for m in $matches; do add_test_file "$m"; done ;;
	esac
done

test_files=$(printf '%s\n' "$test_files" | sed '/^$/d' | sort -u)
if [ -z "$test_files" ]; then
	echo "test-affected: no Go-relevant changes since $base; skipping tests"
	exit 0
fi

names=$(grep -hoE '^func (Test|Fuzz|Example)[A-Za-z0-9_]*\(' $test_files |
	sed -E 's/^func //; s/\($//' | grep -vx TestMain | sort -u | paste -sd'|' -)
if [ -z "$names" ]; then
	echo "test-affected: changed test files define no tests; skipping"
	exit 0
fi

echo "test-affected: files" $test_files
exec go test ${extra[@]+"${extra[@]}"} -run "^(${names})\$" ./...
