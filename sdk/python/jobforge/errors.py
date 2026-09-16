"""JobForge SDK error types.

Error hierarchy mirrors the server-side error codes from ADR-0002.
"""

from __future__ import annotations

from collections.abc import Callable


class JobForgeError(Exception):
    """Base exception for all JobForge SDK errors.

    Attributes:
        code: Machine-readable error code (e.g., "NOT_FOUND").
        message: Human-readable error message.
    """

    def __init__(self, code: str, message: str) -> None:
        self.code = code
        self.message = message
        self.status_code: int | None = None
        super().__init__(f"[{code}] {message}")

    @property
    def retryable(self) -> bool:
        """Whether a caller may retry, with its own bounded policy."""
        return self.code in (
            "QUEUE_OVERLOADED",
            "RATE_LIMITED",
            "DEPENDENCY_UNAVAILABLE",
            "INTERNAL",
            "TRANSPORT",
            "TIMEOUT",
        )


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


class CancelRequestedError(JobForgeError):
    """Cancellation won the race with completion (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("CANCEL_REQUESTED", message)


class InvalidTransitionError(JobForgeError):
    """The operation is not allowed in the current state (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("INVALID_TRANSITION", message)


class TransportError(JobForgeError):
    """The HTTP exchange failed; submission acceptance may be unknown."""

    def __init__(self, message: str = "HTTP exchange failed") -> None:
        super().__init__("TRANSPORT", message)


class RequestTimeoutError(TransportError):
    """The request timed out; retry submissions with the same idempotency key."""

    def __init__(self, message: str = "HTTP request timed out") -> None:
        JobForgeError.__init__(self, "TIMEOUT", message)


class InternalError(JobForgeError):
    """Internal server error (HTTP 500)."""

    def __init__(self, message: str) -> None:
        super().__init__("INTERNAL", message)


class RateLimitedError(JobForgeError):
    """The caller's bounded request rate was exceeded (HTTP 429)."""

    def __init__(self, message: str) -> None:
        super().__init__("RATE_LIMITED", message)


class DependencyUnavailableError(JobForgeError):
    """A required dependency is temporarily unavailable (HTTP 503)."""

    def __init__(self, message: str) -> None:
        super().__init__("DEPENDENCY_UNAVAILABLE", message)


class ProfileUnavailableError(JobForgeError):
    """The exact immutable execution profile is unavailable (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("PROFILE_UNAVAILABLE", message)


class BudgetExhaustedError(JobForgeError):
    """A shared budget cannot authorize new execution (HTTP 409)."""

    def __init__(self, message: str) -> None:
        super().__init__("BUDGET_EXHAUSTED", message)


# Mapping from error code to exception factory. Mapped subclasses accept a
# single message argument and pin their own code, so the map is typed as a
# callable instead of type[JobForgeError].
_ERROR_MAP: dict[str, Callable[[str], JobForgeError]] = {
    "INVALID_ARGUMENT": InvalidArgumentError,
    "UNAUTHORIZED": UnauthorizedError,
    "FORBIDDEN": ForbiddenError,
    "NOT_FOUND": NotFoundError,
    "CONFLICT": ConflictError,
    "ALREADY_TERMINAL": AlreadyTerminalError,
    "STALE_LEASE": StaleLeaseError,
    "CANCEL_REQUESTED": CancelRequestedError,
    "INVALID_TRANSITION": InvalidTransitionError,
    "QUEUE_OVERLOADED": QueueOverloadedError,
    "INTERNAL": InternalError,
    "RATE_LIMITED": RateLimitedError,
    "DEPENDENCY_UNAVAILABLE": DependencyUnavailableError,
    "PROFILE_UNAVAILABLE": ProfileUnavailableError,
    "BUDGET_EXHAUSTED": BudgetExhaustedError,
}


def from_response(code: str, message: str) -> JobForgeError:
    """Create the appropriate exception from an error code and message."""
    exc_factory = _ERROR_MAP.get(code)
    if exc_factory is None:
        return InternalError("unrecognized server error")
    return exc_factory(message)
