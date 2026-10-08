"""Use the original fixed Linux launcher and installed S5 SDK driver."""

from tools.support_evaluation.launcher import main

if __name__ == "__main__":
    raise SystemExit(main(s5=True))
