#!/usr/bin/env python3
# excess.py RUNDIR: per probe, pre-update median latency and the worst excess pause in the window
# (request probes: latency - median; stream probes: gap - median gap); failures with their times.
import json,sys,datetime,pathlib
run=pathlib.Path(sys.argv[1]);ev=[json.loads(l) for l in open(run/'events.jsonl')]
ts=lambda p:datetime.datetime.fromisoformat((run/p).read_text().strip()).timestamp()
u0,u1=ts('update-start.utc'),ts('update-end.utc')
f=lambda t:datetime.datetime.fromtimestamp(t,datetime.timezone.utc).strftime('%H:%M:%S.%f')[:-3]
for feat in ['exec_short','exec_long','exec_stdin','exec_stdin_large','ssh_stream','ssh_connect','send','preview','tunnel','term_cli','term_web']:
  e=[x for x in ev if x['f']==feat]
  if feat in('exec_long','ssh_stream','term_cli','term_web'):
    ok=sorted(x['t'] for x in e if x['ok']);g=[(b-a,a) for a,b in zip(ok,ok[1:])]
    base=sorted(d for d,a in g if a<u0-1);med=base[len(base)//2] if base else 0
    w=[(d-med,a) for d,a in g if u0-30<=a<=u1+60];worst=max(w) if w else (0,0)
  else:
    base=sorted(x['ms']/1000 for x in e if x['ok'] and x['t']<u0-1);med=base[len(base)//2] if base else 0
    w=[(x['ms']/1000-med,x['t']-x['ms']/1000) for x in e if u0-30<=x['t']<=u1+60];worst=max(w) if w else (0,0)
  bad=[f(x['t']) for x in e if not x['ok']]
  print('%-17s base_med=%6.3fs worst_excess=%6.3fs from %s  fails=%d %s'%(feat,med,worst[0],f(worst[1]) if worst[1] else '-',len(bad),(bad[0]+'..'+bad[-1]) if bad else ''))
