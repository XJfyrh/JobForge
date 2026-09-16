# Deterministic adapter checks only: no cloud credentials, model or business gold.
FROM python:3.12.14-slim-bookworm@sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254
WORKDIR /checks
COPY tools/requirements-lint.txt /checks/tools/requirements-lint.txt
RUN python -m pip install --no-cache-dir --requirement tools/requirements-lint.txt
COPY python /checks/python
COPY api/executor /checks/api/executor
USER 65532:65532
ENV PYTHONDONTWRITEBYTECODE=1
CMD ["python", "-m", "pytest", "-p", "no:cacheprovider", "python/tests", "-q"]
