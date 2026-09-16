FROM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build
WORKDIR /src
COPY go.mod go.sum /src/
COPY tools/executorprobe/*.go /src/tools/executorprobe/
COPY internal/runclock/*.go /src/internal/runclock/
RUN CGO_ENABLED=1 go test -race -c -o /out/probe.test ./tools/executorprobe
RUN CGO_ENABLED=1 go test -race -c -o /out/clock.test ./internal/runclock
RUN CGO_ENABLED=0 go build -o /out/executorprobe ./tools/executorprobe

FROM python:3.12.14-slim-bookworm@sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254
WORKDIR /probe
COPY --from=build /out/executorprobe /out/probe.test /out/clock.test /probe/
COPY tools/executorprobe/executor.py /probe/
COPY tools/executorprobe/testdata /probe/testdata/
USER 65532:65532
ENV JOBFORGE_EXECUTOR_PROCESS_TESTS=1
CMD ["./probe.test", "-test.v", "-test.timeout=90s"]
