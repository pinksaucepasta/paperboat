#!/usr/bin/env python3
# mint-token.py NAME OUTFILE: one-shot enrollment token for machine NAME on the test account, written 0600 to
# OUTFILE and never printed. Authenticates with the harness API token (JSON file QV_WEBTOKEN with a "secret"
# field, the one the browser-terminal rows use) against QV_SERVER, so no paired CLI is needed.
import json,os,sys,urllib.request
os.umask(0o077)
name,out=sys.argv[1:3]
tok=json.load(open(os.environ['QV_WEBTOKEN']))['secret']
srv=os.environ['QV_SERVER']
def call(m,p,b=None,h={}):
 r=urllib.request.Request(srv+p,method=m,data=json.dumps(b).encode() if b is not None else None,headers={'Authorization':'Bearer '+tok,'Content-Type':'application/json',**h})
 with urllib.request.urlopen(r,timeout=30) as x:return json.load(x)['data']
t=call('POST','/v1/machine-enrollments',{'shell':'posix'},{'Idempotency-Key':os.urandom(16).hex()})['bootstrap_token']
fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600);os.write(fd,t.encode());os.close(fd)
print('enrollment token for',name,'written')
