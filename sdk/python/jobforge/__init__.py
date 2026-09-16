"""JobForge Python SDK.

A minimal Python client for the JobForge distributed task orchestration platform.

Usage:
    from jobforge import JobForgeClient

    client = JobForgeClient(base_url="http://localhost:8080", api_key="dev-api-key")
    job = client.submit(queue="default", type="demo.echo", payload={"message": "hello"})
    print(job.job_id, job.state)
"""

from jobforge.client import JobForgeClient
from jobforge.errors import (
    AlreadyTerminalError,
    BudgetExhaustedError,
    CancelRequestedError,
    ConflictError,
    DependencyUnavailableError,
    ForbiddenError,
    InternalError,
    InvalidArgumentError,
    InvalidTransitionError,
    JobForgeError,
    NotFoundError,
    ProfileUnavailableError,
    QueueOverloadedError,
    RateLimitedError,
    RequestTimeoutError,
    StaleLeaseError,
    TransportError,
    UnauthorizedError,
)
from jobforge.models import Job, JobState
from jobforge.run_calls import CallBudget, CallUsage, ProviderAudit, RunCall, RunCalls
from jobforge.run_client import RunClient
from jobforge.run_models import (
    BudgetAccount,
    Run,
    RunBudget,
    RunCancellation,
    RunError,
    RunEvent,
    RunEventPage,
    RunPage,
    RunResult,
    RunState,
    RunStep,
    RunStepPage,
    RunSubmission,
    RunUsage,
    VersionVector,
)

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
    "CancelRequestedError",
    "ForbiddenError",
    "InternalError",
    "InvalidTransitionError",
    "RequestTimeoutError",
    "StaleLeaseError",
    "TransportError",
    "UnauthorizedError",
    "RunClient",
    "RunCalls",
    "RunCall",
    "CallBudget",
    "CallUsage",
    "ProviderAudit",
    "Run",
    "RunState",
    "RunError",
    "RunSubmission",
    "RunCancellation",
    "RunPage",
    "RunStep",
    "RunStepPage",
    "RunEvent",
    "RunEventPage",
    "RunResult",
    "RunBudget",
    "RunUsage",
    "BudgetAccount",
    "VersionVector",
    "RateLimitedError",
    "DependencyUnavailableError",
    "ProfileUnavailableError",
    "BudgetExhaustedError",
]

__version__ = "0.1.0"
