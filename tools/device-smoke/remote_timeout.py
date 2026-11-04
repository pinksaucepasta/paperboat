"""The `timeout 120` words that bound pb on one side of a device check; macOS has no timeout(1)."""
import functools
import shlex
import subprocess


@functools.lru_cache(maxsize=None)
def timeout_words(prefix):
    done = subprocess.run(shlex.split(prefix) + ['uname -s'], capture_output=True, text=True, timeout=60)
    if done.returncode != 0:
        raise RuntimeError('could not detect the host OS through: ' + prefix)
    # shortcut: macOS runs pb unbounded remotely; the local subprocess timeout still ends the check.
    return [] if done.stdout.strip() == 'Darwin' else ['timeout', '120']
