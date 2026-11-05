#!/usr/bin/env python3
# code.py TOKENFILE SERVER SESSION_ID OUTFILE: enable interactive join-code access on one terminal session
# (as the owner, by API token); the code goes only to OUTFILE (0600), never to stdout.
import sys,json,os,urllib.request,urllib.error
secret=json.load(open(sys.argv[1]))['secret']
srv,sid,out=sys.argv[2].rstrip('/'),sys.argv[3],sys.argv[4]
def call(method,body=None):
  r=urllib.request.Request(f'{srv}/v1/terminal-sessions/{sid}/code-access',method=method,data=json.dumps(body).encode() if body else None,
    headers={'Authorization':'Bearer '+secret,'Content-Type':'application/json'})
  try:
    with urllib.request.urlopen(r,timeout=20) as resp:return json.load(resp)['data']
  except urllib.error.HTTPError as e:
    err=json.load(e).get('error',{});raise SystemExit(f'{method} code-access: HTTP {e.code} {err.get("code")} {str(err.get("message"))[:160]}')
cur=call('GET')
v=call('PUT',dict(expected_generation=cur['generation'],enabled=True,role='interactive',expires_in_seconds=8*3600,reset=False,confirm_interactive_access=True))
assert v.get('enabled') and v.get('code'),'code access not enabled'
fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600);os.write(fd,v['code'].encode());os.close(fd)
print('code access enabled, role',v['role'])
