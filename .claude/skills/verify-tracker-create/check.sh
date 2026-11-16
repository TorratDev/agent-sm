#!/usr/bin/env bash
# Check the tracker-create rules on the real code:  check.sh [deep|stale]
#   (no argument)  every schedule of up to 2 faults on a one-item plan, 1 fault on a three-item plan
#   deep           up to 3 faults on the one-item plan (about 10 minutes)
#   stale          list covered files that changed since the commit in SYNC; exit 1 if any did
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
cd "$(git -C "$here" rev-parse --show-toplevel)"
covered="management.go management_apply.go management_remote.go management_graph.go management_verify.go management_test.go"
case ${1:-} in
stale)
  # shellcheck disable=SC2086
  changed=$(git diff --stat "$(cat "$here/SYNC")" -- $covered)
  [ -z "$changed" ] && { echo "in step: no covered file changed since $(cat "$here/SYNC")"; exit 0; }
  echo "covered files changed since $(cat "$here/SYNC"):"; echo "$changed"; exit 1
  ;;
deep) export FORMAL_FAULTS_ONE=3 ;;
"") ;;
*) echo "usage: $0 [deep|stale]" >&2; exit 2 ;;
esac
# The journal fsyncs on every save: on a disk the default run takes minutes, on tmpfs seconds.
[ -d /dev/shm ] && export TMPDIR=/dev/shm
exec go test -tags formal -run TestFormalTrackerCreate -count=1 -v -timeout 90m .
