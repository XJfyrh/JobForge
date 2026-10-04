# Mechanism checks use the same installed package and fixed entry as production.
FROM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY proto ./proto
COPY migrations ./migrations
RUN CGO_ENABLED=1 go test -race -c -o /out/process.test ./internal/runexecutor

FROM build AS integration-build
COPY tests ./tests
COPY cmd ./cmd
COPY tools/supportrecoveryproxy ./tools/supportrecoveryproxy
RUN CGO_ENABLED=1 go test -race -c -o /out/integration.test ./tests/integration
RUN CGO_ENABLED=1 go test -race -c -o /out/worker.test ./internal/runworker
RUN CGO_ENABLED=1 go build -race -o /out/agent-worker ./cmd/agent-worker && CGO_ENABLED=1 go build -race -o /out/agent-control ./cmd/agent-control && CGO_ENABLED=1 go build -race -o /out/supportrecoveryproxy ./tools/supportrecoveryproxy

FROM python:3.12.14-slim-bookworm@sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254 AS runtime
WORKDIR /app
COPY tools/requirements-lint.txt /tmp/requirements-lint.txt
RUN python -m pip install --no-cache-dir -r /tmp/requirements-lint.txt hatchling==1.27.0
COPY python/pyproject.toml /tmp/package/pyproject.toml
COPY python/jobforge_agent /tmp/package/jobforge_agent
RUN python -m pip install --no-cache-dir --no-deps --no-build-isolation /tmp/package
ENV PYTHONDONTWRITEBYTECODE=1

FROM runtime AS process-check
COPY --from=build /out/process.test /app/process.test
COPY internal/runexecutor/testdata /src/internal/runexecutor/testdata
COPY api/executor /src/api/executor
WORKDIR /src/internal/runexecutor
USER 65532:65532
ENV JOBFORGE_RUNEXECUTOR_PROCESS_TESTS=1
CMD ["/app/process.test", "-test.v", "-test.timeout=180s"]

FROM runtime AS python-check
WORKDIR /checks
COPY ruff.toml mypy.ini /checks/
COPY python /checks/python
COPY api/executor /checks/api/executor
COPY api/support /checks/api/support
USER 65532:65532
CMD ["python", "-m", "pytest", "-p", "no:cacheprovider", "python/tests", "-q"]

# The following image is explicitly synthetic and is never a production parent.
# It installs a static fixture adapter and static loopback provider binding.
FROM runtime AS integration-check
COPY --from=integration-build /out/integration.test /out/worker.test /app/
COPY --from=integration-build /out/agent-worker /out/agent-control /usr/local/bin/
COPY --from=integration-build /out/supportrecoveryproxy /usr/local/bin/
COPY sdk/python /tmp/sdk
RUN python -m pip install --no-cache-dir --no-deps --no-build-isolation /tmp/sdk
COPY sdk/python/tests/run_recovery_http_contract.py /src/sdk/python/tests/run_recovery_http_contract.py
COPY tools/support_evaluation/launcher.py tools/support_evaluation/driver.py tools/support_evaluation/export.py /usr/local/lib/python3.12/site-packages/tools/support_evaluation/
COPY tools/support_evaluation/assemble.py tools/support_evaluation/evidence.py tools/support_evaluation/validate_data.py /usr/local/lib/python3.12/site-packages/tools/support_evaluation/
COPY tools/support_recovery /usr/local/lib/python3.12/site-packages/tools/support_recovery/
COPY tools/agentruntimecheck/recovery_supervisor_check.py /usr/local/lib/python3.12/site-packages/recovery_supervisor_check.py
COPY migrations /src/migrations
COPY api/executor /src/api/executor
COPY api/support /src/api/support
COPY internal/run/postgres/ledger.go internal/run/postgres/provider_audit_guard.go internal/run/postgres/recovery_guard.go /src/internal/run/postgres/
COPY python/tests/runtime_fixture_registry.py /tmp/runtime_fixture_registry.py
COPY tools/agentruntimecheck/test-manifest.json /tmp/test-manifest.json
COPY tools/agentruntimecheck/test_install.py /tmp/test_install.py
COPY tools/agentruntimecheck/runtime_recovery_faults.py /tmp/runtime_recovery_faults.py
RUN python /tmp/test_install.py
RUN mkdir -p /etc/jobforge/cloud /etc/jobforge/recovery /run/secrets /var/lib/jobforge/batches /var/lib/jobforge/exports && chown -R 65532:65532 /etc/jobforge/cloud /etc/jobforge/recovery /run/secrets /var/lib/jobforge && chown 65532:65532 /etc/jobforge/executor.json
WORKDIR /src/tests/integration
USER 65532:65532
ENV JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1
ENV JOBFORGE_TEST_PYTHON=/usr/local/bin/python
CMD ["/app/integration.test", "-test.v", "-test.run=^TestRun(Executor|SupportExecutor|SupportAgentExecutor|SupportLauncher|ProviderAuditExecutor|Recovery)", "-test.timeout=1200s"]
