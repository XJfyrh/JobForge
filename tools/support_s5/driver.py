"""Use the original one-submit SDK driver with an explicit S5 list size."""

from tools.support_evaluation.driver import main as batch_main
from tools.support_s5.telemetry import setup, stop


def main() -> int:
    """Configure the external SDK tracer before the unchanged finite driver."""
    provider = setup()
    try:
        return batch_main(s5=True)
    finally:
        if provider is not None:
            stop(provider)


if __name__ == "__main__":
    raise SystemExit(main())
