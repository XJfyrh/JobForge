# Mechanism checks use the same installed package and fixed entry as production.
ARG BASE_REGISTRY=docker.io
FROM ${BASE_REGISTRY}/library/golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build
WORKDIR /src
# Bind source and cache compilers instead of copying the entire Go base at each
# COPY layer on vfs-backed Docker hosts. No source or gold enters runtime layers.
RUN --mount=type=secret,id=build_ca \
    if [ -f /run/secrets/build_ca ]; then cp /run/secrets/build_ca /usr/local/share/ca-certificates/build-proxy.crt && update-ca-certificates; fi
RUN --mount=type=bind,target=/src \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go test -race -c -o /out/process.test ./internal/runexecutor

FROM build AS integration-build
RUN --mount=type=bind,target=/src \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go test -race -c -o /out/integration.test ./tests/integration && \
    CGO_ENABLED=1 go test -race -c -o /out/worker.test ./internal/runworker && \
    CGO_ENABLED=1 go build -race -o /out/agent-worker ./cmd/agent-worker && \
    CGO_ENABLED=1 go build -race -o /out/agent-control ./cmd/agent-control

FROM ${BASE_REGISTRY}/library/python:3.12.14-slim-bookworm@sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254 AS runtime
WORKDIR /app
RUN --mount=type=secret,id=build_ca \
    --mount=type=bind,target=/source \
    if [ -f /run/secrets/build_ca ]; then cp /run/secrets/build_ca /usr/local/share/ca-certificates/build-proxy.crt && update-ca-certificates; fi && \
    python -m pip install --no-cache-dir -r /source/tools/requirements-lint.txt hatchling==1.27.0 && \
    python -m pip install --no-cache-dir --no-deps --no-build-isolation /source/python
ENV PYTHONDONTWRITEBYTECODE=1

FROM runtime AS process-check
COPY --from=build /out/process.test /app/process.test
COPY internal/runexecutor/testdata /src/internal/runexecutor/testdata
COPY api/executor /src/api/executor
WORKDIR /src/internal/runexecutor
# Saved workspaces may have mode 0600 files; fixtures are non-secret inputs.
RUN chmod -R a+rX /src
USER 65532:65532
ENV JOBFORGE_RUNEXECUTOR_PROCESS_TESTS=1
CMD ["/app/process.test", "-test.v", "-test.timeout=180s"]

FROM runtime AS python-check
WORKDIR /checks
COPY ruff.toml mypy.ini /checks/
COPY python /checks/python
COPY api/executor /checks/api/executor
COPY api/support /checks/api/support
RUN chmod -R a+rX /checks
USER 65532:65532
CMD ["python", "-m", "pytest", "-p", "no:cacheprovider", "python/tests", "-q"]

# The following image is explicitly synthetic and is never a production parent.
# It installs a static fixture adapter and static loopback provider binding.
FROM runtime AS integration-check
# One materialized layer keeps vfs hosts from cloning the runtime for every fixture.
RUN --mount=type=bind,target=/source \
    --mount=type=bind,from=integration-build,source=/out,target=/compiled \
    cp /compiled/integration.test /compiled/worker.test /app/ && \
    cp /compiled/agent-worker /compiled/agent-control /usr/local/bin/ && \
    python -m pip install --no-cache-dir --no-deps --no-build-isolation /source/sdk/python && \
    mkdir -p /usr/local/lib/python3.12/site-packages/tools/support_evaluation /src/api /src/tests/integration && \
    cp /source/tools/support_evaluation/launcher.py /source/tools/support_evaluation/driver.py /source/tools/support_evaluation/export.py /usr/local/lib/python3.12/site-packages/tools/support_evaluation/ && \
    cp -r /source/migrations /src/migrations && \
    cp -r /source/api/executor /source/api/support /src/api/ && \
    cp /source/python/tests/runtime_fixture_registry.py /tmp/ && \
    cp /source/tools/agentruntimecheck/test-manifest.json /source/tools/agentruntimecheck/test_install.py /tmp/ && \
    python /tmp/test_install.py && \
    mkdir -p /etc/jobforge/cloud /run/secrets /var/lib/jobforge/batches /var/lib/jobforge/exports && \
    chown -R 65532:65532 /etc/jobforge/cloud /run/secrets /var/lib/jobforge && \
    chown 65532:65532 /etc/jobforge/executor.json && \
    chmod -R a+rX /src /usr/local/lib/python3.12/site-packages/tools
WORKDIR /src/tests/integration
USER 65532:65532
ENV JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1
CMD ["/app/integration.test", "-test.v", "-test.run=^TestRun(Executor|SupportExecutor|SupportAgentExecutor|SupportLauncher|ProviderAuditExecutor)", "-test.timeout=420s"]
