"""Stable, content-free errors for the business adapter boundary."""


class ToolError(Exception):
    """Expose a fixed error code without retaining HTTP bodies or credentials."""

    def __init__(self, code: str) -> None:
        """Create a sanitized protocol or dependency failure."""
        super().__init__(code)
        self.code = code
