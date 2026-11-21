# ct_env.sh: sourced by the harness scripts that create or drive throwaway systemd containers on a Docker host.
# One place for the container settings: SMOKE_CT_IMAGE (default test-systemd), SMOKE_CT_USER (pbt), SMOKE_CT_UID (1000),
# SMOKE_CT_DOCKER (default "sudo -n docker"; "docker" on hosts whose user needs no sudo). Python twin: ct_env.py.
CT_IMAGE=${SMOKE_CT_IMAGE:-test-systemd} CT_USER=${SMOKE_CT_USER:-pbt} CT_UID=${SMOKE_CT_UID:-1000} CT_DOCKER=${SMOKE_CT_DOCKER:-sudo -n docker}
[[ $CT_IMAGE =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*$ && $CT_USER =~ ^[a-z_][a-z0-9_-]*$ && $CT_UID =~ ^[0-9]+$ && $CT_DOCKER =~ ^[a-z\ -]+$ ]] \
  || { echo "bad SMOKE_CT_IMAGE/USER/UID/DOCKER ($CT_IMAGE $CT_USER $CT_UID $CT_DOCKER)" >&2; exit 2; }
CT_HOME=/home/$CT_USER CT_RUNDIR=/run/user/$CT_UID
export CT_IMAGE CT_USER CT_UID CT_DOCKER CT_HOME CT_RUNDIR
# ct_env_remote: the lines a script piped to `ssh HOST bash -s` needs first, so its remote shell sees the same settings.
ct_env_remote() { printf 'export CT_IMAGE=%q CT_USER=%q CT_UID=%q CT_DOCKER=%q CT_HOME=%q CT_RUNDIR=%q\n' "$CT_IMAGE" "$CT_USER" "$CT_UID" "$CT_DOCKER" "$CT_HOME" "$CT_RUNDIR"; }
