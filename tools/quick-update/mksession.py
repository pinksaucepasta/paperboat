#!/usr/bin/env python3
# mksession.py TARGET NAME (in the controller): create a durable terminal session NAME on TARGET, detach, print only its id.
import sys,re
sys.path.insert(0,'/tmp/smoke')
from terminal_lifecycle import Terminal
from session_cleanup import inventory
tgt,name=sys.argv[1:3];pb='/home/pbt/.local/bin/pb'
assert re.fullmatch(r'smoke-[A-Za-z0-9_-]+',name)
t=Terminal([pb,'connect',tgt,'--name',name,'--status-bar','off'])
try:t.expect(b'\x1b[?2004h',timeout=90)
finally:t.close()
_,sessions=inventory(pb,tgt)
ids=[s['id'] for s in sessions if s.get('name')==name and s.get('state')!='closed']
assert len(ids)==1,'session not listed once'
print(ids[0])
