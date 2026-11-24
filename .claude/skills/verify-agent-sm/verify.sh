#!/usr/bin/env bash
# One isolated agent-sm web instance for verification: launch | doctor | api | evidence | stop.
# State lives outside the checkout (a real run needs a clean working tree):
#   $VERIFY_HOME/instance  scratch, removed by stop
#   $VERIFY_HOME/evidence  proof artifacts, never removed
set -euo pipefail

REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
VERIFY_HOME=${VERIFY_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/agent-sm-verify}
INST=$VERIFY_HOME/instance
BIN=$INST/agent-sm

port() { cat "$INST/port" 2>/dev/null || echo "${VERIFY_PORT:-8799}"; }
url() { echo "http://127.0.0.1:$(port)"; }
rev() { echo "$(git -C "$REPO" rev-parse HEAD)$(git -C "$REPO" diff --quiet HEAD -- . ':!.claude' || echo +dirty)"; }
# Ours means: the recorded pid is alive and is running the binary we built.
ours() {
  [ -f "$INST/pid" ] || return 1
  local pid; pid=$(cat "$INST/pid")
  [ "$(readlink "/proc/$pid/exe" 2>/dev/null)" = "$BIN" ]
}
evidence() { local d; d=$VERIFY_HOME/evidence/$(cat "$INST/run"); mkdir -p "$d"; echo "$d"; }

case ${1:-} in
launch)
  if ours; then echo "already running at $(url); run '$0 stop' first" >&2; exit 1; fi
  p=${VERIFY_PORT:-8799}
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    echo "port $p is taken by something else; set VERIFY_PORT" >&2; exit 1
  fi
  rm -rf "$INST"; mkdir -p "$INST"
  echo "$p" >"$INST/port"; date +%Y%m%d-%H%M%S >"$INST/run"; rev >"$INST/rev"
  (cd "$REPO" && go build -o "$BIN" .)
  # Seed: an archived run of issue 359 (attempt3 ended "ready"; attempt1 has denied commands),
  # placed where the page looks for this repo's runs.
  seedrun=$REPO/runs/359/${2:-attempt3}
  [ -f "$seedrun/log.jsonl" ] || { echo "no archived run at $seedrun" >&2; exit 1; }
  seed=$INST/state/repos/$(printf %s "$REPO" | sha256sum | cut -c1-16)/359
  mkdir -p "$seed"
  cp "$seedrun/log.jsonl" "$seed/events.jsonl"
  cp "$seedrun/stats.jsonl" "$INST/stats.jsonl"
  cd "$REPO" # serve auto-selects the repo it is started in
  env -u SM_SCRATCH -u SM_EVENTS -u SM_PUBLISH -u SM_SELF \
    SM_SCRATCH_ROOT="$INST/state" SM_STATS="$INST/stats.jsonl" \
    setsid "$BIN" serve "127.0.0.1:$p" >"$INST/server.log" 2>&1 &
  echo $! >"$INST/pid"
  for _ in $(seq 50); do
    if curl -fs "$(url)/api/state" 2>/dev/null | grep -q '"issue":"359"'; then
      echo "ready: $(url)  evidence: $(evidence)"; exit 0
    fi
    sleep 0.2
  done
  echo "not ready after 10s; see $INST/server.log" >&2; exit 1
  ;;
doctor)
  bad=0
  check() { if eval "$2" >/dev/null 2>&1; then echo "ok    $1"; else echo "FAIL  $1"; bad=1; fi; }
  check "process is ours (pid file -> $BIN)" ours
  check "page answers at $(url)" 'curl -fs "$(url)/" | grep -q "<title>agent-sm</title>"'
  check "graph has transitions" 'curl -fs "$(url)/api/graph" | grep -q "\"from\":\"prepare\""'
  check "seeded run 359 is shown" 'curl -fs "$(url)/api/state" | grep -q "\"issue\":\"359\""'
  check "build matches checkout ($(cat "$INST/rev" 2>/dev/null))" '[ "$(cat "$INST/rev")" = "$(rev)" ]'
  exit $bad
  ;;
api) # api GET /api/graph   |   api POST /api/folders '{"path":"/tmp"}'
  ours || { echo "no instance of ours; run '$0 launch'" >&2; exit 1; }
  token=$(curl -fs "$(url)/" | sed -n 's/^const TOKEN = "\(.*\)";$/\1/p')
  curl -sS -w '\nHTTP %{http_code}\n' -X "$2" -H "X-SM-Token: $token" -H 'Content-Type: application/json' \
    ${4:+-d "$4"} "$(url)$3"
  ;;
evidence) evidence ;;
stop)
  [ -d "$INST" ] || { echo "nothing to stop"; exit 0; }
  ev=$(evidence)
  if ours; then
    pid=$(cat "$INST/pid"); kill "$pid"
    for _ in $(seq 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  fi
  cp "$INST/server.log" "$ev/server.log" 2>/dev/null || true
  rm -rf "$INST"
  echo "stopped; evidence kept in $ev"
  ;;
*) echo "usage: $0 launch [attempt1|attempt2|attempt3|attempt4]|doctor|api METHOD PATH [JSON]|evidence|stop" >&2; exit 2 ;;
esac
