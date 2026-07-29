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
  if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  if [[ -n "$runner_pid" ]] && kill -0 "$runner_pid" 2>/dev/null; then
    kill -TERM "$runner_pid" 2>/dev/null || true
    wait "$runner_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | tail -n +2 | grep -q .; then
  echo "refusing to use occupied port $PORT" >&2
  exit 1
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

for _ in $(seq 1 100); do
  [[ -S "$STATE/$id.sock" && -f "$STATE/$id.json" && -f "$STATE/$id.events" && -f "$STATE/$id.log" ]] && break
  sleep 0.05
done
[[ -S "$STATE/$id.sock" ]]
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

for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
  sleep 0.05
done
curl -fsS "http://127.0.0.1:$PORT/api/health"
echo

# The protected request creates an auth token under SCRATCH_HOME only.
curl -sS "http://127.0.0.1:$PORT/api/sessions" >/dev/null
token=$(tr -d '\r\n' <"$SCRATCH_HOME/.local/state/pretty-PTY/token")
auth="Authorization: Bearer $token"

sessions=
for _ in $(seq 1 100); do
  sessions=$(curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions")
  if /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"; then
    break
  fi
  sleep 0.05
done
echo '$ curl -H "Authorization: Bearer <scratch-token>" http://127.0.0.1:8898/api/sessions'
echo "$sessions"
/opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"

echo '$ curl -X POST .../api/sessions/<id>/input --data {"data":"echo INTEROP_<random>\\r"}'
input_result=$(curl -fsS -H "$auth" -H 'Content-Type: application/json' \
  -X POST "http://127.0.0.1:$PORT/api/sessions/$id/input" \
  --data-binary "{\"data\":\"echo $marker\\r\"}")
echo "$input_result"

snapshot=/tmp/gorunner-snapshot.txt
for _ in $(seq 1 100); do
  curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions/$id/snapshot" >"$snapshot"
  grep -Fq "$marker" "$snapshot" && break
  sleep 0.05
done
echo '$ curl .../snapshot | grep -o "INTEROP_[0-9]*" | tail -1'
grep -ao 'INTEROP_[0-9]*' "$snapshot" | tail -1
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
wait "$daemon_pid"
daemon_pid=
kill -0 "$runner_pid"
test -S "$STATE/$id.sock"
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
for _ in $(seq 1 100); do
  sessions=$(curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions" 2>/dev/null || true)
  if /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); try { process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1) } catch { process.exit(1) }' "$sessions" "$id"; then
    break
  fi
  sleep 0.05
done
echo "sessions_after_reattach=$sessions"
/opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && !s.exited) ? 0 : 1)' "$sessions" "$id"
curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions/$id/snapshot" >"$snapshot"
grep -Fq "$marker" "$snapshot"
echo "snapshot_replay_after_reattach=$marker"

echo '$ curl -X DELETE .../api/sessions/<id>'
kill_result=$(curl -fsS -H "$auth" -X DELETE "http://127.0.0.1:$PORT/api/sessions/$id")
echo "$kill_result"

exited=
for _ in $(seq 1 100); do
  exited=$(curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions?include_exited=1")
  if /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && s.exited) ? 0 : 1)' "$exited" "$id"; then
    break
  fi
  sleep 0.05
done
echo "exit_record=$exited"
/opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id && s.exited) ? 0 : 1)' "$exited" "$id"

for _ in $(seq 1 100); do
  sessions=$(curl -fsS -H "$auth" "http://127.0.0.1:$PORT/api/sessions")
  if ! /opt/homebrew/bin/node -e 'const [j,id]=process.argv.slice(1); process.exit(JSON.parse(j).sessions.some(s => s.id === id) ? 0 : 1)' "$sessions" "$id"; then
    break
  fi
  sleep 0.05
done
echo "sessions_after_kill=$sessions"

# runner.ts keeps an exited runner for a 30 second reconnect grace. Prove the
# matching Go lifecycle eventually removes its live state while retaining log.
for _ in $(seq 1 350); do
  kill -0 "$runner_pid" 2>/dev/null || break
  sleep 0.1
done
set +e
wait "$runner_pid"
runner_status=$?
set -e
runner_pid=
echo "runner_exit_status=$runner_status"
echo '$ find /tmp/gorunner-state -maxdepth 1 -type f -o -type s'
find "$STATE" -maxdepth 1 \( -type f -o -type s \) -print | sort
[[ -f "$STATE/$id.log" ]]
[[ ! -e "$STATE/$id.sock" && ! -e "$STATE/$id.json" && ! -e "$STATE/$id.events" ]]

echo '$ tail -n 5 /tmp/gorunner-daemon.out'
tail -n 5 "$DAEMON_OUT"
