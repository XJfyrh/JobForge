#!/usr/bin/env bash
# Synthetic model; real Go, PostgreSQL, gRPC, Python and installed SDK.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
all=false
if [[ $# -eq 1 && $1 == --all ]]; then all=true
elif [[ $# -ne 0 ]]; then echo 'Usage: bash tools/demo-support.sh [--all]' >&2; exit 2
fi
docker info >/dev/null
mkdir -p .cache/verification
report=$(mktemp -d "$PWD/.cache/verification/demo-XXXXXXXX")
export BUILDX_CONFIG="${BUILDX_CONFIG:-$PWD/.cache/buildx}"
image="jobforge-support-demo:$(basename "$report")"
args=(--target integration-check -f tools/agentruntimecheck/Dockerfile -t "$image")
if [[ -n ${JOBFORGE_BUILD_REGISTRY:-} ]]; then args+=(--build-arg "BASE_REGISTRY=$JOBFORGE_BUILD_REGISTRY"); fi
# Optional public proxy CA only, never an API key or an authorization credential.
if [[ -n ${JOBFORGE_BUILD_CA:-} ]]; then args+=(--secret "id=build_ca,src=$JOBFORGE_BUILD_CA"); fi
containers=()
network=""
cleanup() {
  local result=$?
  trap - EXIT
  for container in "${containers[@]}"; do
    docker rm -fv "$container" >/dev/null || result=1
  done
  if [[ -n "$network" ]]; then docker network rm "$network" >/dev/null || result=1; fi
  echo "Demo evidence: $report"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo 'Building the current source (first build downloads pinned toolchains)...'
if ! docker build "${args[@]}" . >"$report/build.log" 2>&1; then
  tail -20 "$report/build.log" >&2
  exit 1
fi
network=$(docker network create --internal "$(basename "$report")")
pg=$(docker create --network "$network" --network-alias postgres \
  --label jobforge.purpose=support-demo -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test \
  -e POSTGRES_DB=jobforge_test postgres:16-alpine)
containers+=("$pg")
docker start "$pg" >/dev/null
for ((i=0;i<60;i++)); do
  if docker exec "$pg" pg_isready -U test -d jobforge_test >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$pg" pg_isready -U test -d jobforge_test >/dev/null
command=(/app/integration.test -test.v '-test.run=^TestRunSupportAgentExecutor$/^dynamic$' -test.timeout=90s)
if $all; then
  command=(/bin/sh -c '/app/worker.test -test.v -test.timeout=120s && /app/integration.test -test.v "-test.run=^TestRun(Executor|SupportExecutor|SupportAgentExecutor|SupportLauncher|ProviderAuditExecutor)" -test.timeout=420s')
fi
runner=$(docker create --init --network "$network" --add-host control:127.0.0.1 \
  --label jobforge.purpose=support-demo --cpus 2 --memory 512m --pids-limit 96 \
  -e JOBFORGE_TEST_DSN=postgres://test:test@postgres:5432/jobforge_test?sslmode=disable \
  "$image" "${command[@]}")
containers+=("$runner")
echo 'Running synthetic delivery case; external network and paid credentials are absent...'
if ! docker start -a "$runner" >"$report/demo.log" 2>&1; then cat "$report/demo.log"; exit 1; fi
result=$(docker inspect --format '{{.State.ExitCode}}' "$runner")
cat "$report/demo.log"
[[ "$result" == 0 ]] || exit 1
grep -q 'DEMO .*"synthetic_model": true' "$report/demo.log" || { echo 'Demo result missing' >&2; exit 1; }
