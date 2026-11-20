"""Read development targets from the operator's local configuration."""

import json
import os
from pathlib import Path
import re
import sys

DEFAULT_PATH = Path('~/.config/paperboat-dev/targets.json')
PATH_ENV = 'PB_DEV_TARGETS_FILE'


def load():
    override = os.environ.get(PATH_ENV)
    path = Path(override or DEFAULT_PATH).expanduser()
    try:
        with path.open() as source:
            settings = json.load(source)
    except FileNotFoundError:
        if not override:
            return {}
        raise SystemExit(f'development targets: {PATH_ENV} points to a missing file') from None
    except (OSError, UnicodeError, ValueError):
        raise SystemExit('development targets: cannot read local JSON configuration') from None
    if not isinstance(settings, dict):
        raise SystemExit('development targets: expected a JSON object')
    return settings


def smoke_environment(settings):
    variables = settings.get('smoke', {})
    if not isinstance(variables, dict) or any(
        not re.fullmatch(r'(?:SMOKE_|PAPERBOAT_SMOKE_)[A-Z0-9_]+', key)
        or not isinstance(value, str) or '\0' in value for key, value in variables.items()
    ):
        raise SystemExit('development targets: invalid smoke settings')
    aliases = settings.get('smoke_env_aliases', {})
    if not isinstance(aliases, dict) or any(not isinstance(key, str) or not isinstance(alias, str)
                                             for key, alias in aliases.items()):
        raise SystemExit('development targets: invalid environment aliases')
    overrides = {key: os.environ[alias] for key, alias in aliases.items() if alias in os.environ}
    env = {**variables, **overrides, **os.environ}
    if 'results_dir' in settings:
        env['SMOKE_RESULTS_DIR'] = str(configured_path(settings, 'results_dir'))
    return env


def configured_path(settings, key, default=None):
    value = settings.get(key)
    if value is None:
        return default
    if not isinstance(value, str) or not value:
        raise SystemExit(f'development targets: {key} must be a path')
    return Path(value).expanduser()


def get_setting(key, default=''):
    return load().get(key, default)


def smoke_value(key, default=''):
    if key in os.environ:
        return os.environ[key]
    return smoke_environment(load()).get(key, default)


def device_names():
    env = smoke_environment(load())
    return [env.get('SMOKE_' + role + '_NAME', fallback) for role, fallback in (
        ('LINUX', 'linux'), ('MACOS', 'macos'), ('WINDOWS', 'windows'),
        ('PEER', 'linux-peer'), ('TEAMMATE', 'teammate'),
    )]


def main():
    settings = load()
    args = sys.argv[1:]
    if len(args) > 1 and args[0] == 'run':
        os.execvpe(args[1], args[1:], smoke_environment(settings))
    if len(args) == 2 and args[0] == 'ssh':
        hosts = settings.get('remote_hosts', {})
        if not isinstance(hosts, dict):
            raise SystemExit('development targets: remote_hosts must be an object')
        host = hosts.get(args[1], args[1])
        if not isinstance(host, str) or not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.:@-]*', host):
            raise SystemExit('development targets: invalid SSH target')
        print(host)
    elif len(args) == 2 and args[0] == 'smoke':
        value = smoke_environment(settings).get(args[1], '')
        if args[1] not in {'SMOKE_WINDOWS_SSH', 'SMOKE_WINDOWS_HOME'}:
            raise SystemExit('development targets: load passwords through the smoke launcher')
        print(value)
    elif args == ['windows_name']:
        print(smoke_environment(settings).get('SMOKE_WINDOWS_NAME', 'windows'))
    elif len(args) == 1:
        value = settings.get(args[0], '')
        if not isinstance(value, str):
            raise SystemExit('development targets: expected a string setting')
        print(os.path.expanduser(value))
    else:
        raise SystemExit('development targets: expected a setting name or ssh followed by a target')


if __name__ == '__main__':
    main()
