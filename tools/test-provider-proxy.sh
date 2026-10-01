#!/usr/bin/env bash
# Installed formal Worker through its coordinator; synthetic credentials only.
set -euo pipefail
ca_file=$(realpath "${1:?usage: test-provider-proxy.sh PUBLIC_CA_FILE [IMAGE]}")
image=${2:-jobforge-review-runtime:provider-fixed}
network="jobforge-proxy-offline-$$"
network_created=false
pg=""
runner=""
cleanup() {
  if [[ -n "$runner" ]]; then docker rm -fv "$runner" >/dev/null 2>&1 || true; fi
  if [[ -n "$pg" ]]; then docker rm -fv "$pg" >/dev/null 2>&1 || true; fi
  if [[ "$network_created" == true ]]; then docker network rm "$network" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
docker network create --internal "$network" >/dev/null
network_created=true
pg=$(docker create --network "$network" --network-alias db -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=jobforge_test postgres:16-alpine)
docker start "$pg" >/dev/null
for ((i=0;i<30;i++)); do
  if docker exec "$pg" pg_isready -h 127.0.0.1 -U test -d jobforge_test >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$pg" pg_isready -h 127.0.0.1 -U test -d jobforge_test >/dev/null
proxy_env=()
for key in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy; do proxy_env+=(-e "$key="); done
runner=$(docker create --init --network "$network" --cpus 2 --memory 512m --pids-limit 96 --ulimit core=0 \
  "${proxy_env[@]}" --mount "type=bind,src=$ca_file,dst=/opt/jobforge-provider-ca.pem,readonly" \
  -e JOBFORGE_TEST_DSN='postgres://test:test@db:5432/jobforge_test?sslmode=disable' \
  -e JOBFORGE_PROVIDER_PROXY_OFFLINE=1 --entrypoint /app/integration.test "$image" \
  -test.v -test.run='^TestRunProviderProxyOffline$' -test.timeout=180s)
docker start --attach "$runner"
[[ $(docker inspect --format '{{.State.ExitCode}}' "$runner") == 0 ]]
