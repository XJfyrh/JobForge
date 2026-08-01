"""JobForge Python SDK.

A minimal Python client for the JobForge distributed task orchestration platform.

Usage:
    from jobforge import JobForgeClient

    client = JobForgeClient(base_url="http://localhost:8080", api_key="dev-api-key")
    job = client.submit(queue="default", type="demo.echo", payload={"message": "hello"})
    print(job.id, job.state)
"""

from jobforge.client import JobForgeClient
from jobforge.errors import (
    AlreadyTerminalError,
    ConflictError,
    InvalidArgumentError,
    JobForgeError,
    NotFoundError,
    QueueOverloadedError,
)
from jobforge.models import Job, JobState

__all__ = [
    "JobForgeClient",
    "Job",
    "JobState",
    "JobForgeError",
    "InvalidArgumentError",
    "NotFoundError",
    "ConflictError",
    "QueueOverloadedError",
    "AlreadyTerminalError",
]

__version__ = "0.1.0"
