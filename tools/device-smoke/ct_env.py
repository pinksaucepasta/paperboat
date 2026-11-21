"""The throwaway containers' image, user, uid and docker command, from SMOKE_CT_* (see ct_env.sh, its shell twin)."""
import os
import re

CT_IMAGE = os.environ.get('SMOKE_CT_IMAGE', 'test-systemd')
CT_USER = os.environ.get('SMOKE_CT_USER', 'pbt')
CT_UID = os.environ.get('SMOKE_CT_UID', '1000')
CT_DOCKER = os.environ.get('SMOKE_CT_DOCKER', 'sudo -n docker')
if not (re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._/:-]*', CT_IMAGE) and re.fullmatch(r'[a-z_][a-z0-9_-]*', CT_USER)
        and re.fullmatch(r'[0-9]+', CT_UID) and re.fullmatch(r'[a-z -]+', CT_DOCKER)):
    raise SystemExit(f'bad SMOKE_CT_IMAGE/USER/UID/DOCKER ({CT_IMAGE} {CT_USER} {CT_UID} {CT_DOCKER})')
CT_HOME = f'/home/{CT_USER}'
CT_RUNDIR = f'/run/user/{CT_UID}'
# `docker exec` flags that run a command as the container's user
CT_EXEC = f'-u {CT_USER} -e HOME={CT_HOME} -e XDG_RUNTIME_DIR={CT_RUNDIR}'
