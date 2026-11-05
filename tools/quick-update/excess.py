#!/usr/bin/env python3
# excess.py RUNDIR: re-score a run. The verdict per feature is the extra pause or latency the update caused:
# the worst value among calls/gaps that overlap [update_start-LO, update_end+HI], minus that feature's median
# before the window; it passes at <=LIMIT s with 0 failures. Anything outside the window is noise context only.
#  - request loops (exec, ssh_connect, send, preview, tunnel): value = that call's own latency, so a loop's
#    cadence (exec_stdin_large starts one every 10 s) is never counted as a pause;
#  - streams (exec_long, ssh_stream, term_cli, term_web): value = gap between consecutive markers.
# send: `pb send` exposes no progress, so the probe sends a small file back to back and each call's latency is
# the measure (a stall inside a send lengthens that call).
import json,sys,datetime,pathlib
LO,HI,LIMIT=2.0,10.0,1.0
FEATS=['exec_short','exec_long','exec_stdin','exec_stdin_large','ssh_stream','ssh_connect','send','preview','tunnel','term_cli','term_web']
STREAMS={'exec_long','ssh_stream','term_cli','term_web'}
iso=lambda s:datetime.datetime.fromisoformat(s.strip()).timestamp()
hms=lambda t:datetime.datetime.fromtimestamp(t,datetime.timezone.utc).strftime('%H:%M:%S.%f')[:-3]

def load_events(run):
 """events.jsonl + the held CLI terminal's markers (term_cli) + the browser terminal's events (term_web)."""
 import time
 ev=[json.loads(l) for l in open(run/'events.jsonl')]
 try:
  hr=json.loads((run/'held-result.json').read_text() or '{}');hm=json.loads((run/'held-markers.json').read_text() or '[]')
  ev+=[dict(f='term_cli',t=iso(m['at']),ok=True,ms=None) for m in hm]
  if hr.get('failure_type'):ev.append(dict(f='term_cli',t=iso(hr.get('failure_at') or hr['finished']),ok=False,ms=None,why=hr['failure_type']))
 except Exception as e:ev.append(dict(f='term_cli',t=time.time(),ok=False,ms=None,why='no held result: '+type(e).__name__))
 if (run/'web-events.jsonl').exists():ev+=[json.loads(l) for l in open(run/'web-events.jsonl')]
 return ev

def items(e,stream):
 """(start,end,seconds) of every call, or of every gap between consecutive successful markers."""
 if stream:
  ts=sorted(x['t'] for x in e if x['ok']);return [(a,b,b-a) for a,b in zip(ts,ts[1:])]
 return [(x['t']-x['ms']/1000,x['t'],x['ms']/1000) for x in e if x['ms'] is not None]  # a failed call's wait counts too

def score(ev,updates):
 """updates: [(start,end)] unix seconds. One row per feature; ok = excess<=LIMIT and no failures."""
 win=[(a-LO,b+HI) for a,b in updates];lo,hi=min(w[0] for w in win),max(w[1] for w in win)
 inw=lambda s,e:any(e>=l and s<=h for l,h in win)
 rows=[]
 for feat in FEATS:
  e=[x for x in ev if x['f']==feat]
  if not e:rows.append(dict(feature=feat,events=0,ok=False));continue
  it=items(e,feat in STREAMS);base=sorted(v for s,t,v in it if t<lo);med=base[len(base)//2] if base else 0
  w=[(v-med,s) for s,t,v in it if inw(s,t)];worst=max(w) if w else (None,None)
  pre=[v-med for s,t,v in it if t<lo];post=[v-med for s,t,v in it if s>hi]
  bad=[x for x in e if not x['ok']]
  starts=[x for x in ev if x['f']==feat+'_start'];ends=[x for x in ev if x['f']==feat+'_end']
  fin=len([x for x in bad if inw(x['t']-(x['ms'] or 0)/1000,x['t'])])
  ex=None if worst[0] is None else round(max(0,worst[0]),3)
  rows.append(dict(feature=feat,events=len(e),restarts=max(0,len(starts)-1),cut=[x.get('rc') for x in ends],base_med_s=round(med,3),
   excess_s=ex,excess_at=hms(worst[1]) if worst[1] else None,in_window=len(w),pre_max_s=round(max(0,max(pre,default=0)),3),post_max_s=round(max(0,max(post,default=0)),3),
   failures=len(bad),failures_in_update=fin,ok=ex is not None and ex<=LIMIT and not bad))
 return rows

def render(rows,updates):
 lo,hi=min(a for a,b in updates)-LO,max(b for a,b in updates)+HI
 print('window %s..%s UTC (update start-%gs .. end+%gs); verdict = worst in-window excess over pre-window median, pass <=%gs and 0 failures'%(hms(lo),hms(hi),LO,HI,LIMIT))
 print('%-17s %8s %9s %9s %9s %6s %5s  %s'%('feature','base_med','excess','pre_max','post_max','calls','fails','verdict'))
 for r in rows:
  if not r.get('events'):print('%-17s %s'%(r['feature'],'NO EVENTS  FAIL'));continue
  x=lambda k:'-' if r.get(k) is None else '%.3fs'%r[k]
  print('%-17s %8s %9s %9s %9s %6d %5d  %s'%(r['feature'],x('base_med_s'),x('excess_s'),x('pre_max_s'),x('post_max_s'),r['in_window'],r['failures'],'PASS' if r['ok'] else 'FAIL'+(' (no data in window)' if r['excess_s'] is None else '')))

if __name__=='__main__':
 run=pathlib.Path(sys.argv[1]);u=[(iso((run/'update-start.utc').read_text()),iso((run/'update-end.utc').read_text()))]
 render(score(load_events(run),u),u)
