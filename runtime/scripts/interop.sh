#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
GO=/opt/homebrew/bin/go
STATE=/tmp/gorunner-state
SCRATCH_HOME=/tmp/gorunner-home
WORK=/tmp/gorunner-work
PORT=8898
RUNNER_BIN=/tmp/runtime-runner-interop
RUNNER_OUT=/tmp/gorunner-runner.out
DAEMON_OUT=/tmp/gorunner-daemon.out

runner_pid=
daemon_pid=
cleanup() {
  local status=$?
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  if [[ -n "$runner_pid" ]] && kill -0 "$runner_pid" 2>/dev/null; then
    kill -TERM "$runner_pid" 2>/dev/null || true
    wait "$runner_pid" 2>/dev/null || true
  fi
  # The runner log is the only record of why the runner side failed. It used to
  # be captured to $RUNNER_OUT and never read by anything, so an interop failure
  # discarded exactly the evidence needed to explain it.
  if [[ "$status" -ne 0 ]]; then
    echo "interop failed (exit $status); captured process logs follow" >&2
    local log
    for log in "$RUNNER_OUT" "$DAEMON_OUT"; do
      if [[ -f "$log" ]]; then
        printf -- '--- %s (last 50 lines) ---\n' "$log" >&2
        tail -n 50 "$log" >&2 || true
      else
        printf -- '--- %s (not created) ---\n' "$log" >&2
      fi
    done
  fi
  return $status
}
trap cleanup EXIT

# Under `set -e` a bare `[[ ... ]]` or `grep -q` ends the script with exit 1 and
# no sentence. The cleanup trap then prints two process logs and leaves the
# reader to work out which of the twenty checks above produced them. `require`
# makes each check say what it was checking.
require() {
  local description="$1"; shift
  if ! "$@"; then
    echo "interop: FAILED — $description" >&2
    echo "interop: the check that failed was: $*" >&2
    return 1
  fi
}

# Every poll in this script was `for _ in $(seq 1 100); sleep 0.05` — bounded,
# so it could not hang, but silent when it ran out. `await` keeps the bound and
# adds the sentence, so "the runner never published its socket" is not reported
# as "line 123".
#
# The budgets are wall-clock seconds where the originals counted attempts. 100
# attempts each also paid a curl round-trip, so on a loaded machine they waited
# well past five seconds; the 15s deadlines below are chosen to be no stricter
# than what they replace. Naming a failure must not create new ones.
await() {
  local description="$1" seconds="$2"; shift 2
  local deadline=$((SECONDS + seconds))
  while ((SECONDS < deadline)); do
    if "$@"; then return 0; fi
    sleep 0.05
  done
  echo "interop: TIMED OUT after ${seconds}s waiting for $description" >&2
  echo "interop: the condition that never became true was: $*" >&2
  return 1
}

# `wait` on a process that ignores the signal it was sent blocks forever. That
# turns a failing interop run into a hung one, which is strictly worse: a hang
# has no exit code to report and no log to read. Bound it, and say so.
# AWAIT_EXIT_STATUS carries the waited process's own exit status, which the
# script reports; the function's return value is only whether it exited in time.
AWAIT_EXIT_STATUS=0
await_exit() {
  local pid="$1" description="$2" seconds="$3"
  local deadline=$((SECONDS + seconds))
  while ((SECONDS < deadline)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      set +e; wait "$pid" 2>/dev/null; AWAIT_EXIT_STATUS=$?; set -e
      return 0
    fi
    sleep 0.1
  done
  echo "interop: TIMED OUT after ${seconds}s waiting for $description (pid $pid) to exit" >&2
  kill -KILL "$pid" 2>/dev/null || true
  set +e; wait "$pid" 2>/dev/null; AWAIT_EXIT_STATUS=$?; set -e
  return 1
}

# `lsof` missing or failing must never be read as "the port is free": the next
# thing this script does is `rm -rf` fixed state directories, and doing that
# while a previous run's daemon/runner is still live destroys its state out from
# under it. Treat only an explicit "no listener" answer as free.
if ! command -v lsof >/dev/null 2>&1; then
  echo "interop: lsof is required to prove port $PORT is free before wiping state" >&2
  exit 1
fi
set +e
lsof_output="$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null)"
lsof_status=$?
set -e
# Drop lsof's header row; whatever remains is a real listener.
port_listeners="$(printf '%s\n' "$lsof_output" | tail -n +2)"
# lsof exits 1 both for "nothing matched" and for some errors; it only counts as
# "free" when it also produced no output. Any other status is unexplained.
if [[ "$lsof_status" -gt 1 ]]; then
  echo "interop: lsof failed (exit $lsof_status); cannot prove port $PORT is free" >&2
  exit 1
fi
if [[ -n "$port_listeners" ]]; then
  echo "refusing to use occupied port $PORT" >&2
  printf '%s\n' "$port_listeners" >&2
  exit 1
fi

# Even with the port free, a previous run's runner can still be holding sockets
# in the fixed state directory. Removing those files would strand a live
# process, so refuse rather than wipe.
if [[ -d "$STATE" ]]; then
  live_state_sockets=""
  for state_socket in "$STATE"/*.sock; do
    [[ -S "$state_socket" ]] || continue
    if lsof -- "$state_socket" >/dev/null 2>&1; then
      live_state_sockets="${live_state_sockets}${state_socket}"$'\n'
    fi
  done
  if [[ -n "$live_state_sockets" ]]; then
    echo "interop: refusing to wipe $STATE; these sockets are still open by a live process:" >&2
    printf '%s' "$live_state_sockets" >&2
    echo "interop: stop the leftover runner/daemon first" >&2
    exit 1
  fi
fi

# These are fixed disposable paths, never the user's default Sessions state.
rm -rf "$STATE" "$SCRATCH_HOME" "$WORK"
mkdir -p "$STATE" "$SCRATCH_HOME" "$WORK"

echo '$ CGO_ENABLED=0 /opt/homebrew/bin/go build -o /tmp/runtime-runner-interop ./cmd/sessions-runner'
(
  cd "$ROOT/runtime"
  CGO_ENABLED=0 "$GO" build -o "$RUNNER_BIN" ./cmd/sessions-runner
)

id=$(/usr/bin/uuidgen | tr '[:upper:]' '[:lower:]')
marker="INTEROP_${RANDOM}"
echo "session_id=$id"
echo "marker=$marker"

env -i \
  HOME="$SCRATCH_HOME" \
  PATH=/opt/homebrew/bin:/usr/bin:/bin \
  LANG=en_US.UTF-8 \
  SHELL=/bin/bash \
  RUNNER_ID="$id" \
  RUNNER_STATE_DIR="$STATE" \
  RUNNER_CMD=/bin/bash \
  RUNNER_ARGS_JSON='["-i"]' \
  RUNNER_CWD="$WORK" \
  "$RUNNER_BIN" >"$RUNNER_OUT" 2>&1 &
runner_pid=$!
echo "runner_pid=$runner_pid"

await 'the Go runner to publish its socket, record, event log and output log' 15 \
  test -S "$STATE/$id.sock" -a -f "$STATE/$id.json" -a -f "$STATE/$id.events" -a -f "$STATE/$id.log"
require 'the runner control socket to exist' test -S "$STATE/$id.sock"
echo '$ ls -l /tmp/gorunner-state'
ls -l "$STATE"

echo '$ HOME=/tmp/gorunner-home PRETTYD_STATE_DIR=/tmp/gorunner-state PRETTYD_PORT=8898 node runtime/testdata/node-runtime/dist/server.js'
env -i \
  HOME="$SCRATCH_HOME" \
  PATH=/opt/homebrew/bin:/usr/bin:/bin \
  LANG=en_US.UTF-8 \
  PRETTYD_STATE_DIR="$STATE" \
  PRETTYD_PORT="$PORT" \
  /opt/homebrew/bin/node "$ROOT/runtime/testdata/node-runtime/dist/server.js" >"$DAEMON_OUT" 2>&1 &
daemon_pid=$!
echo "daemon_pid=$daemon_pid"

daemon_healthy() { curl -fsS --max-time 2 "http://127.0.0.1:$PORT/api/health" -o /dev/null 2>/dev/null; }
await 'the TypeScript daemon to answer /api/health' 15 daemon_healthy
require 'the daemon health endpoint to answer' \
  curl -fsS --max-time 5 "http://127.0.0.1:$PORT/api/health"
echo

# The protected request creates an auth token under SCRATCH_HOME only.
curl -sS "http://127.0.0.1:$PORT/api/sessions" >/dev/null
token=$(tr -d '\r\n' <"$SCRATCH_HOME/.local/state/pretty-PTY/token")
auth="Authorization: Bearer $token"

sessions=
live_session_visible() {
  sessions=$(curl -fsS --max-time 5 -H "$auth" "http://127.0.0.1:$PORT/api/sessions") || return 1
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"
}
await "the TypeScript daemon to discover the Go runner's session $id as live" 15 live_session_visible
echo '$ curl -H "Authorization: Bearer <scratch-token>" http://127.0.0.1:8898/api/sessions'
echo "$sessions"
require 'the daemon to list the Go runner session as live' \
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"

echo '$ curl -X POST .../api/sessions/<id>/input --data {"data":"echo INTEROP_<random>\\r"}'
input_result=$(curl -fsS -H "$auth" -H 'Content-Type: application/json' \
  -X POST "http://127.0.0.1:$PORT/api/sessions/$id/input" \
  --data-binary "{\"data\":\"echo $marker\\r\"}")
echo "$input_result"

snapshot=/tmp/gorunner-snapshot.txt
marker_in_snapshot() {
  curl -fsS --max-time 5 -H "$auth" "http://127.0.0.1:$PORT/api/sessions/$id/snapshot" >"$snapshot" || return 1
  grep -Fq "$marker" "$snapshot"
}
await "the echoed marker $marker to appear in the session snapshot" 15 marker_in_snapshot
echo '$ curl .../snapshot | grep -o "INTEROP_[0-9]*" | tail -1'
grep -ao 'INTEROP_[0-9]*' "$snapshot" | tail -1
require "the snapshot to contain the marker $marker the shell was asked to echo" \
  grep -Fq "$marker" "$snapshot"

echo '$ existing TypeScript PersistentLog.restoreFrom(<go-events-file>)'
MODULE="$ROOT/runtime/testdata/node-runtime/dist/persistentLog.js" EVENTS="$STATE/$id.events" MARKER="$marker" \
  /opt/homebrew/bin/node --input-type=module -e '
    const { PersistentLog } = await import("file://" + process.env.MODULE);
    const events = PersistentLog.restoreFrom(process.env.EVENTS);
    const found = events.some(event => event.data.includes(process.env.MARKER));
    console.log(`ts_restore_events=${events.length} ts_restore_marker=${found ? "yes" : "no"}`);
    process.exit(found ? 0 : 1);
  '

echo '$ kill -TERM <daemon-pid>; test -S /tmp/gorunner-state/<id>.sock'
kill -TERM "$daemon_pid"
await_exit "$daemon_pid" 'the TypeScript daemon' 15
daemon_pid=
require 'the Go runner to survive the daemon disconnect' kill -0 "$runner_pid"
require 'the runner control socket to survive the daemon disconnect' test -S "$STATE/$id.sock"
echo 'runner_survived_daemon_disconnect=yes'

echo '$ restart the same isolated TS daemon and rediscover the runner'
env -i \
  HOME="$SCRATCH_HOME" \
  PATH=/opt/homebrew/bin:/usr/bin:/bin \
  LANG=en_US.UTF-8 \
  PRETTYD_STATE_DIR="$STATE" \
  PRETTYD_PORT="$PORT" \
  /opt/homebrew/bin/node "$ROOT/runtime/testdata/node-runtime/dist/server.js" >"$DAEMON_OUT" 2>&1 &
daemon_pid=$!
rediscovered() {
  sessions=$(curl -fsS --max-time 5 -H "$auth" "http://127.0.0.1:$PORT/api/sessions" 2>/dev/null || true)
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); try { process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1) } catch { process.exit(1) }' "$sessions" "$id"
}
await 'the restarted daemon to rediscover the surviving runner' 15 rediscovered
echo "sessions_after_reattach=$sessions"
require 'the restarted daemon to list the surviving runner as live' \
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"
curl -fsS --max-time 10 -H "$auth" "http://127.0.0.1:$PORT/api/sessions/$id/snapshot" >"$snapshot"
require 'the snapshot to replay the pre-disconnect marker after reattach' \
  grep -Fq "$marker" "$snapshot"
echo "snapshot_replay_after_reattach=$marker"

echo '$ curl -X DELETE .../api/sessions/<id>'
kill_result=$(curl -fsS -H "$auth" -X DELETE "http://127.0.0.1:$PORT/api/sessions/$id")
echo "$kill_result"

exited=
exit_recorded() {
  exited=$(curl -fsS --max-time 5 -H "$auth" "http://127.0.0.1:$PORT/api/sessions?include_exited=1") || return 1
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && s.exited) ? 0 : 1)' "$exited" "$id"
}
await 'the deleted session to appear in the exited list' 15 exit_recorded
echo "exit_record=$exited"
require 'the exited session to carry an exit record' \
  /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && s.exited) ? 0 : 1)' "$exited" "$id"

dropped_from_live_list() {
  sessions=$(curl -fsS --max-time 5 -H "$auth" "http://127.0.0.1:$PORT/api/sessions") || return 1
  ! /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id) ? 0 : 1)' "$sessions" "$id"
}
await 'the killed session to drop out of the live session list' 15 dropped_from_live_list
echo "sessions_after_kill=$sessions"

# runner.ts keeps an exited runner for a 30 second reconnect grace. Prove the
# matching Go lifecycle eventually removes its live state while retaining log.
# `wait` here used to be unbounded: if the runner never exited, the script
# stopped forever with nothing printed. Bound it, name it, and kill it so the
# run ends with a reportable failure instead of a hang.
await_exit "$runner_pid" 'the Go runner to exit after its reconnect grace' 40
runner_status=$AWAIT_EXIT_STATUS
runner_pid=
echo "runner_exit_status=$runner_status"
echo '$ find /tmp/gorunner-state -maxdepth 1 -type f -o -type s'
find "$STATE" -maxdepth 1 \( -type f -o -type s \) -print | sort
require 'the output log to be retained after the runner exits' test -f "$STATE/$id.log"
require 'the live-state files (socket, record, events) to be removed after the runner exits' \
  test ! -e "$STATE/$id.sock" -a ! -e "$STATE/$id.json" -a ! -e "$STATE/$id.events"

echo '$ tail -n 5 /tmp/gorunner-daemon.out'
tail -n 5 "$DAEMON_OUT"

echo '$ tail -n 5 /tmp/gorunner-runner.out'
tail -n 5 "$RUNNER_OUT"
