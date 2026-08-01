"""JobForge SDK error types.

Error hierarchy mirrors the server-side error codes from ADR-0002.
"""

from __future__ import annotations


class JobForgeError(Exception):
    """Base exception for all JobForge SDK errors.

    Attributes:
        code: Machine-readable error code (e.g., "NOT_FOUND").
        message: Human-readable error message.
    """

    def __init__(self, code: str, message: str) -> None:
        self.code = code
        self.message = message
        super().__init__(f"[{code}] {message}")


class InvalidArgumentError(JobForgeError):
    """Request parameters are invalid (HTTP 400)."""

    def __init__(self, message: str) -> None:
        super().__init__("INVALID_ARGUMENT", message)


class UnauthorizedError(JobForgeError):
    """Authentication failed (HTTP 401)."""

    def __init__(self, message: str) -> None:
        super().__init__("UNAUTHORIZED", message)


class ForbiddenError(JobForgeError):
    """Permission denied (HTTP 403)."""

    def __init__(self, message: str) -> None:
        super().__init__("FORBIDDEN", message)


class NotFoundError(JobForgeError):
    """Resource not found (HTTP 404)."""

    def __init__(self, message: str) -> None:
        super().__init__("NOT_FOUND", message)


class ConflictError(JobForgeError):
    """Idempotency key conflict (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("CONFLICT", message)


class AlreadyTerminalError(JobForgeError):
    """Job is in a terminal state (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("ALREADY_TERMINAL", message)


class StaleLeaseError(JobForgeError):
    """Fencing token or owner mismatch (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("STALE_LEASE", message)


class QueueOverloadedError(JobForgeError):
    """Queue is at capacity (HTTP 429)."""

    def __init__(self, message: str) -> None:
        super().__init__("QUEUE_OVERLOADED", message)


class InternalError(JobForgeError):
    """Internal server error (HTTP 500)."""

    def __init__(self, message: str) -> None:
        super().__init__("INTERNAL", message)


# Mapping from error code to exception class.
_ERROR_MAP: dict[str, type[JobForgeError]] = {
    "INVALID_ARGUMENT": InvalidArgumentError,
    "UNAUTHORIZED": UnauthorizedError,
    "FORBIDDEN": ForbiddenError,
    "NOT_FOUND": NotFoundError,
    "CONFLICT": ConflictError,
    "ALREADY_TERMINAL": AlreadyTerminalError,
    "STALE_LEASE": StaleLeaseError,
    "QUEUE_OVERLOADED": QueueOverloadedError,
    "INTERNAL": InternalError,
}


def from_response(code: str, message: str) -> JobForgeError:
    """Create the appropriate exception from an error code and message."""
    exc_class = _ERROR_MAP.get(code, JobForgeError)
    if exc_class is JobForgeError:
        return JobForgeError(code, message)
    return exc_class(message)
