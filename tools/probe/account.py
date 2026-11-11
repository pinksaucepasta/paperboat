#!/usr/bin/env python3
"""Production account probe (R-06): a real WorkOS password sign-in, a throwaway `pb auth login` approved by that browser
session and revoked again, and the sign-out. One JSON line per step like probe.sh (step, ok, seconds, error); exit 1 if any failed.
Run every ACCOUNT_EVERY minutes by canary.py (WorkOS sign-ins are rate limited and mail new-device alerts: not every minute).
The browser state and the CLI home live in one private temp directory that is removed on every exit path.

env (the canary unit's EnvironmentFile): ACCOUNT_ORIGIN console origin, ACCOUNT_EMAIL, ACCOUNT_PASSWORD_FILE (read by the node script,
never printed), ACCOUNT_RUN (command prefix that runs its trailing arguments as the pair host's pb user, like RUN_A in probe.sh:
the container made by `CANARY_SERVER=<url> TOKEN_CMD= tools/probe/canary-setup.sh local pb-canary-pair canary-pair-1 <pb>`),
ACCOUNT_SERVER (the control plane URL, passed as --server), PW_CHROMIUM, [PW_MODULE playwright-core index.mjs].
Steps: signin (console_session.mjs password), pair (enrollment token from that session, the pair host joins as canary-pair-<epoch> and is
seen online), login (CLI session revoked, a new `pb auth login` approved by the browser session, auth status signed in), cleanup (every
canary-pair-* machine revoked by exact id, the CLI session logged out; runs on every path, also sweeps what a killed run left), signout (console_signout.mjs: /api/v1/me is 401 afterwards).
"""
import json, os, re, shlex, shutil, subprocess, sys, tempfile, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
SMOKE = HERE.parent / "device-smoke"
PREFIX = "canary-pair-"
NODE = ["nice", "-n", "15", "node"]


def run_step(name, fn):
    start = time.monotonic()
    try:
        fn()
        line = {"step": name, "ok": True, "seconds": round(time.monotonic() - start)}
    except Exception as error:  # a step reports its failure; the next step still runs
        line = {"step": name, "ok": False, "seconds": round(time.monotonic() - start), "error": str(error).strip()[-300:]}
    print(json.dumps(line), flush=True)
    return line["ok"]


def node(script, *args, extra=None, timeout=120):
    env = dict(os.environ, ORIGIN=os.environ["ACCOUNT_ORIGIN"], **(extra or {}))
    done = subprocess.run([*NODE, str(script), *args], env=env, capture_output=True, text=True, timeout=timeout)
    if done.returncode:
        raise RuntimeError(f"{Path(script).name} failed: {(done.stderr or done.stdout)[-200:]}")


def pb_argv(*args):
    return [*shlex.split(os.environ["ACCOUNT_RUN"]), ".local/bin/pb", "--server", os.environ["ACCOUNT_SERVER"], *args]


def pb(*args, timeout=90):
    done = subprocess.run(pb_argv(*args), capture_output=True, text=True, timeout=timeout)
    try:
        body = json.loads(done.stdout.splitlines()[0]) if done.stdout.strip() else {}
    except ValueError:
        body = {}
    return done.returncode, body.get("data", body)


def logged_in():
    rc, status = pb("auth", "status", "--json")
    return rc == 0 and status.get("signed_in") is True


def cli_login(state):
    process = subprocess.Popen(pb_argv("auth", "login", "--no-browser", "--json"),
                               stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        approval = json.loads(process.stdout.readline())
        approval = approval.get("data", approval)
        if approval.get("state") != "approval_required":
            raise RuntimeError(f"login did not ask for browser approval ({approval.get('state')})")
        node(SMOKE / "approve_cli_login.mjs", approval["approval_url"], extra={"STATE": state}, timeout=120)
        if process.wait(timeout=90):
            raise RuntimeError(f"pb auth login exited {process.returncode}: {(process.stdout.read() + process.stderr.read())[-400:]}")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()


def probe_machines():
    rc, listing = pb("machine", "list", "--all", "--json")
    if rc:
        raise RuntimeError("pb machine list failed")
    return [m for m in listing.get("machines", []) if str(m.get("alias", "")).startswith(PREFIX)]


def revoke_probe_machines(state):
    """Disconnect every canary-pair-* machine by exact id through the console session (this run's, or one a killed run left)."""
    node(HERE / "console_machines.mjs", extra={"STATE": state, "PREFIX": PREFIX})


def cleanup(state):
    """On every path: the CLI session is logged out (the server must confirm it) while its machine still exists, then the
    probe machines are revoked through the browser session."""
    problems = []
    rc, out = pb("auth", "logout", "--json")
    if rc or out.get("server_revocation") != "complete":
        problems.append(f"the server did not confirm the CLI session revocation (exit {rc}, {out.get('server_revocation')})")
    try:
        revoke_probe_machines(state)
    except Exception as error:
        problems.append(str(error))
    if problems:
        raise RuntimeError("; ".join(problems))


def pair(state):
    """Add the pair host (a container) as machine canary-pair-<epoch> with a fresh enrollment token, and see it online."""
    revoke_probe_machines(state)  # a killed run may have left its machine: remove it before adding another
    token = subprocess.run([*NODE, str(SMOKE / "mint_enrollment_token.mjs")], env=dict(os.environ, ORIGIN=os.environ["ACCOUNT_ORIGIN"], STATE=state),
                           capture_output=True, text=True, timeout=120)
    if token.returncode:
        raise RuntimeError(f"enrollment token: {token.stderr[-200:]}")
    subprocess.run([*shlex.split(os.environ["ACCOUNT_RUN"]), "sh", "-c", "umask 077; cat > /home/pbt/token; chmod 600 /home/pbt/token"], input=token.stdout, text=True, check=True, timeout=30)
    name = f"{PREFIX}{int(time.time())}"
    try:
        rc, out = pb("pair", "--enrollment-token-file", "/home/pbt/token", "--name", name, "--json", timeout=180)
        if rc and ("pb reset" in json.dumps(out) or "already has a Paperboat machine enrollment" in json.dumps(out)):
            # the previous run's (revoked) enrollment is still on the host: reset discards it so this run pairs afresh
            host = subprocess.run([*shlex.split(os.environ["ACCOUNT_RUN"]), "hostname"], capture_output=True, text=True, timeout=30).stdout.strip()
            rc, out = pb("reset", "--confirmation", "RESET PAPERBOAT", "--hostname", host, "--enrollment-token-file", "/home/pbt/token", "--json", timeout=120)
            if rc:
                raise RuntimeError(f"pb reset exited {rc}: {json.dumps(out)[:300]}")
            rc, out = pb("pair", "--enrollment-token-file", "/home/pbt/token", "--name", name, "--json", timeout=180)
        if rc:
            raise RuntimeError(f"pb pair exited {rc}: {json.dumps(out)[:300]}")
    finally:
        subprocess.run([*shlex.split(os.environ["ACCOUNT_RUN"]), "truncate", "-s0", "/home/pbt/token"], timeout=30)
    machines = [m for m in probe_machines() if m.get("alias") == name]
    if not machines or not machines[0].get("online"):
        raise RuntimeError(f"machine {name} is not online after pairing")


def login():
    """The device login: revoke the CLI session, approve a new `pb auth login` in the signed-in browser, see the session work."""
    rc, out = pb("auth", "logout", "--json")
    if rc or out.get("server_revocation") != "complete":
        raise RuntimeError(f"logout before the login was not confirmed by the server (exit {rc})")
    cli_login(STATE_PATH)
    if not logged_in():
        raise RuntimeError("auth status is not signed in after the approved login")


STATE_PATH = ""


def main():
    global STATE_PATH
    home = tempfile.mkdtemp(prefix="canary-account-")
    os.chmod(home, 0o700)
    STATE_PATH = f"{home}/console-state.json"
    ok = True
    try:
        signed_in = run_step("signin", lambda: node(SMOKE / "console_session.mjs", "password", os.environ["ACCOUNT_EMAIL"],
                                                    os.environ["ACCOUNT_PASSWORD_FILE"], STATE_PATH))
        ok &= signed_in
        if signed_in:
            try:
                paired = run_step("pair", lambda: pair(STATE_PATH))
                ok &= paired
                if paired:
                    ok &= run_step("login", login)
            finally:  # whatever happened: exact-id revocation of the probe machines and the CLI session
                ok &= run_step("cleanup", lambda: cleanup(STATE_PATH))
            ok &= run_step("signout", lambda: node(HERE / "console_signout.mjs", extra={"STATE": STATE_PATH}))
    finally:
        shutil.rmtree(home, ignore_errors=True)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
