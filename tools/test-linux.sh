#!/usr/bin/env bash
# Run service contracts against new, disposable resources only.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [[ "$(uname -s)" != Linux || $# -ne 0 ]]; then
  echo 'Usage: bash tools/test-linux.sh (Linux with local Docker)' >&2
  exit 2
fi
if [[ -x .tools/go/bin/go ]]; then
  export PATH="$PWD/.tools/go/bin:$PATH"
fi
export GOCACHE="${GOCACHE:-$PWD/.cache/go-build}"
export GOMODCACHE="${GOMODCACHE:-$PWD/.cache/go-mod}"
python="$PWD/.venv/bin/python"
[[ -x "$python" ]] || { echo 'Install the .venv toolchain first' >&2; exit 2; }
command -v go >/dev/null
docker info >/dev/null
# Rebuild the installed packages so HTTP subprocess contracts cannot silently
# exercise a stale wheel left in the environment by an earlier checkout.
if "$python" -m pip --version >/dev/null 2>&1; then
  "$python" -m pip install --no-deps --no-build-isolation ./sdk/python ./python
elif command -v uv >/dev/null; then
  uv --cache-dir "$PWD/.cache/uv" pip install --python "$python" \
    --reinstall --no-deps --no-build-isolation ./sdk/python ./python
else
  echo 'Install pip or uv in the development environment first' >&2
  exit 2
fi
"$python" -c 'import jobforge, jobforge_agent, pytest'

# Never inherit a service DSN, cloud endpoint, paid profile or helper flag.
while IFS= read -r variable; do
  case "$variable" in JOBFORGE_*|DEEPSEEK_API_KEY|OPENAI_API_KEY) unset "$variable" ;; esac
done < <(compgen -e)

mkdir -p .cache/verification
report=$(mktemp -d "$PWD/.cache/verification/linux-XXXXXXXX")
containers=()
test_pid=""
cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n "$test_pid" ]]; then
    kill -TERM -- "-$test_pid" 2>/dev/null || true
    wait "$test_pid" 2>/dev/null || true
  fi
  for container in "${containers[@]}"; do
    # IDs come only from docker create below; never discover or prune resources.
    if ! docker rm -fv "$container" >/dev/null; then
      echo "Could not clean owned test container: $container" >&2
      result=1
    fi
  done
  echo "Verification evidence: $report"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start_postgres() {
  local image=$1 container
  container=$(docker create --label jobforge.purpose=isolated-verification \
    -p 127.0.0.1::5432 -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test \
    -e POSTGRES_DB=jobforge_test "$image")
  containers+=("$container")
  docker start "$container" >/dev/null
  for ((attempt=0; attempt<60; attempt++)); do
    if docker exec "$container" pg_isready -U test -d jobforge_test >/dev/null 2>&1; then
      started_container=$container
      return
    fi
    sleep 1
  done
  echo 'Isolated PostgreSQL did not become ready' >&2
  exit 1
}
port() { docker port "$1" "$2" | sed -n 's/^127\.0\.0\.1://p'; }
start_postgres postgres:16-alpine
control_port=$(port "$started_container" 5432)
export JOBFORGE_TEST_DSN="postgres://test:test@127.0.0.1:$control_port/jobforge_test?sslmode=disable"
start_postgres pgvector/pgvector:0.8.6-pg16-bookworm@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b
business_port=$(port "$started_container" 5432)
export JOBFORGE_BUSINESS_TEST_DSN="postgres://test:test@127.0.0.1:$business_port/jobforge_test?sslmode=disable"
# Docker may assign a different ephemeral published port after stop/start.
# Select an available loopback port, then ask Docker to bind that exact port.
# A concurrent bind fails safely at start; it never falls back to an existing broker.
redis_port=$("$python" - <<'PYPORT'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PYPORT
)
redis=$(docker create --label jobforge.purpose=isolated-verification \
  -p "127.0.0.1:$redis_port:6379" redis:7-alpine redis-server --appendonly yes --appendfsync everysec)
containers+=("$redis")
docker start "$redis" >/dev/null
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$redis" redis-cli ping >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$redis" redis-cli ping >/dev/null
redis_port=$(port "$redis" 6379)
export JOBFORGE_TEST_REDIS_URL="redis://127.0.0.1:$redis_port/0"
export JOBFORGE_TEST_REDIS_CONTAINER="$redis"
export JOBFORGE_TEST_PYTHON="$python"

echo 'Running Go race, PostgreSQL, pgvector, Redis and installed Python HTTP contracts...'
status=0
setsid go test -race -count=1 -timeout 30m -json ./... >"$report/go-test.jsonl" 2>"$report/go-test.stderr" &
test_pid=$!
wait "$test_pid" || status=$?
test_pid=""
printf 'Go exit status: %s\n' "$status"
"$python" - "$report/go-test.jsonl" <<'PY'
import json
import sys
from collections import Counter
from pathlib import Path

counts = Counter()
for line in Path(sys.argv[1]).read_text().splitlines():
    record = json.loads(line)
    action = record.get("Action")
    if action in {"pass", "fail", "skip"} and "Test" in record:
        counts[action] += 1
        if action != "pass":
            print(f"{action.upper()}: {record['Package']}/{record['Test']}")
print("Test events:", dict(counts))
print("Dedicated Linux process/runtime, real models and scale are separate layers.")
PY
exit "$status"
