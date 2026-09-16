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
RUN CGO_ENABLED=1 go test -race -c -o /out/integration.test ./tests/integration
RUN CGO_ENABLED=1 go test -race -c -o /out/worker.test ./internal/runworker
RUN CGO_ENABLED=1 go build -race -o /out/agent-worker ./cmd/agent-worker && CGO_ENABLED=1 go build -race -o /out/agent-control ./cmd/agent-control

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
COPY sdk/python /tmp/sdk
RUN python -m pip install --no-cache-dir --no-deps --no-build-isolation /tmp/sdk
COPY tools/support_evaluation/launcher.py tools/support_evaluation/driver.py tools/support_evaluation/export.py /usr/local/lib/python3.12/site-packages/tools/support_evaluation/
COPY migrations /src/migrations
COPY api/executor /src/api/executor
COPY api/support /src/api/support
COPY python/tests/runtime_fixture_registry.py /tmp/runtime_fixture_registry.py
COPY tools/agentruntimecheck/test-manifest.json /tmp/test-manifest.json
COPY tools/agentruntimecheck/test_install.py /tmp/test_install.py
RUN python /tmp/test_install.py
RUN mkdir -p /etc/jobforge/cloud /run/secrets /var/lib/jobforge/batches /var/lib/jobforge/exports && chown -R 65532:65532 /etc/jobforge/cloud /run/secrets /var/lib/jobforge && chown 65532:65532 /etc/jobforge/executor.json
WORKDIR /src/tests/integration
USER 65532:65532
ENV JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1
CMD ["/app/integration.test", "-test.v", "-test.run=^TestRun(Executor|SupportExecutor|SupportAgentExecutor|SupportLauncher|ProviderAuditExecutor)", "-test.timeout=420s"]
