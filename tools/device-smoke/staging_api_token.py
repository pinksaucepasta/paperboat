#!/usr/bin/env python3
"""A one-day API token of the staging test account, on stdout only (for `pb login --api-token-file -` on a machine paired to staging).

Same method as deploy/staging.py enroll: the fixture session cookie is read inside the remote process on the staging host and never
appears in an argv. Usage: staging_api_token.py NAME   (run from a main checkout; `deploy/staging.py seed` has created the account).
"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'deploy'))
import staging  # noqa: E402


def main():
    name = sys.argv[1]
    staging.fixture_credentials()
    program = f"""import json,secrets,urllib.request,urllib.error
from pathlib import Path
c=json.loads(Path('.fixture-auth.json').read_text())
request=urllib.request.Request('http://127.0.0.1:{staging.LOOPBACK}/v1/workspaces/personal/api-tokens',data=json.dumps({{'name':{name!r},'expires_in_days':1}}).encode(),headers={{
 'Cookie':'__Host-pb-session='+c['session']+'; __Host-pb-csrf='+c['csrf'],
 'X-CSRF-Token':c['csrf'],'Origin':{staging.api_url()!r},'Content-Type':'application/json',
 'Idempotency-Key':'staging-token-'+secrets.token_hex(16)}})
try:
    with urllib.request.urlopen(request,timeout=20) as response: print(response.read().decode())
except urllib.error.HTTPError as error:
    print(json.dumps({{'http_status':error.code}}))
"""
    out = staging.remote('python3 -c ' + staging.shell_quote(program))
    try:
        data = json.loads(out)['data']
        sys.stdout.write(data.get('secret') or data['token'])
    except (ValueError, KeyError, TypeError):
        raise SystemExit('staging: the API token for %s was refused (run deploy/staging.py seed first)' % name)


if __name__ == '__main__':
    main()
