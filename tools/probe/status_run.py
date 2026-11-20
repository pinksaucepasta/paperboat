#!/usr/bin/env python3
"""One status probe pass for a vantage point that is not the canary host (database test host, staging): its checks, plus the probe.sh steps in
its config's "probe" block ({"env": {...}, "steps": "step names"}), posted to the status Worker. Run every minute by
deploy/paperboat-status-probe.timer. The unit's EnvironmentFile sets STATUS_PROBE_CONFIG."""
import os, subprocess, sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import canary, status_report  # noqa: E402


def probe_steps(block):
    env = {**os.environ, **block.get("env", {}), "PROBE_STEPS": block["steps"]}
    try:
        done = subprocess.run([str(HERE / "probe.sh")], capture_output=True, text=True, timeout=120, env=env)
        return canary.parse(done.stdout, done.returncode)
    except subprocess.TimeoutExpired:
        return [{"step": "probe", "ok": False, "seconds": 120, "error": "probe.sh did not finish in 120 seconds"}]


def main():
    cfg = status_report.load_config()
    if not cfg:
        sys.exit("STATUS_PROBE_CONFIG is not set")
    steps = probe_steps(cfg["probe"]) if cfg.get("probe") else []
    status_report.report(cfg, status_report.step_results(steps, cfg.get("steps", {})))


if __name__ == "__main__":
    main()
