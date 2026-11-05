#!/usr/bin/env python3
# excess.py RUNDIR: re-score a run. The verdict per feature is the extra pause or latency the update caused:
# the worst value among calls/gaps that overlap [update_start-LO, update_end+HI], minus that feature's median
# before the window; it passes at <=LIMIT s with 0 failures. Above LIMIT it fails, unless the noise outside the window
# (pre/post max) is as high as the in-window excess: then the probe cannot tell, and the row is UNRESOLVED (never PASS).
#  - request loops (exec, ssh_connect, send): value = that call's own latency, so a loop's cadence
#    (exec_stdin_large starts one every 10 s) is never counted as a pause;
#  - streams (exec_long, ssh_stream, term_cli, term_web) and the HTTP fetch loops (preview, tunnel): value = gap
#    between consecutive successful markers/fetches, i.e. the outage between the last success before a pause and
#    the first one after it; a failed or timed-out fetch only makes the gap longer (and counts as a failure).
# send: a 64 KiB file every second; only sends overlapping the window are scored, by their own latency (`pb send`
# exposes no progress, so the pause is the send's whole time, which is small). send_big (20 MB every 10 s) is the
# throughput check: it fails only on failures and reports MB/s before the window and its lowest inside it.
import json,sys,datetime,pathlib,collections
LO,HI,LIMIT=2.0,10.0,1.0
FEATS=['exec_short','exec_long','exec_stdin','exec_stdin_large','ssh_stream','ssh_connect','send','send_big','preview','tunnel','term_cli','term_web']
STREAMS={'exec_long','ssh_stream','term_cli','term_web','preview','tunnel'}
iso=lambda s:datetime.datetime.fromisoformat(s.strip()).timestamp()
hms=lambda t:datetime.datetime.fromtimestamp(t,datetime.timezone.utc).strftime('%H:%M:%S.%f')[:-3]

def load_events(run):
 """events.jsonl + the held CLI terminal's markers (term_cli) + the browser terminal's events (term_web)."""
 import time
 ev=[json.loads(l) for l in open(run/'events.jsonl') if l.strip()];have={x['f'] for x in ev}  # the Mac runner already merged them in
 if 'term_cli' not in have:
  try:
   hr=json.loads((run/'held-result.json').read_text() or '{}');hm=json.loads((run/'held-markers.json').read_text() or '[]')
   ev+=[dict(f='term_cli',t=iso(m['at']),ok=True,ms=None) for m in hm]
   if hr.get('failure_type'):ev.append(dict(f='term_cli',t=iso(hr.get('failure_at') or hr['finished']),ok=False,ms=None,why=hr['failure_type']))
  except Exception as e:ev.append(dict(f='term_cli',t=time.time(),ok=False,ms=None,why='no held result: '+type(e).__name__))
 if 'term_web' not in have and (run/'web-events.jsonl').exists():ev+=[json.loads(l) for l in open(run/'web-events.jsonl') if l.strip()]
 return ev

def items(e,stream):
 """(start,end,seconds) of every call, or of every gap between consecutive successful markers/fetches."""
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
  errs=dict(collections.Counter(x.get('err') or ('exit%s'%x['rc'] if 'rc' in x else 'unrecorded') for x in bad))  # codes only
  starts=[x for x in ev if x['f']==feat+'_start'];ends=[x for x in ev if x['f']==feat+'_end']
  fin=len([x for x in bad if inw(x['t']-(x['ms'] or 0)/1000,x['t'])])
  ex=None if worst[0] is None else round(max(0,worst[0]),3)
  rows.append(dict(feature=feat,events=len(e),restarts=max(0,len(starts)-1),cut=[x.get('rc') for x in ends],base_med_s=round(med,3),
   excess_s=ex,excess_at=hms(worst[1]) if worst[1] else None,in_window=len(w),pre_max_s=round(max(0,max(pre,default=0)),3),post_max_s=round(max(0,max(post,default=0)),3),
   failures=len(bad),failures_in_update=fin,errors=errs))
  r=rows[-1];r['noise_s']=max(r['pre_max_s'],r['post_max_s'])
  r['status']='FAIL' if ex is None or bad else 'PASS' if ex<=LIMIT else 'UNRESOLVED' if r['noise_s']>=ex else 'FAIL'
  if feat=='send_big':  # throughput check: only failures fail it
   mb=[(x['bytes']/1e6/(x['ms']/1000),x['t']) for x in e if x['ok'] and x.get('bytes') and x['ms']]
   before=sorted(v for v,t in mb if t<lo);inwin=[v for v,t in mb if lo<=t<=hi]
   r.update(status='FAIL' if bad else 'PASS',info='%s MB/s before, lowest in window %s'%('%.1f'%before[len(before)//2] if before else '-','%.1f'%min(inwin) if inwin else '-'))
  r['ok']=r['status']=='PASS'
 return rows

def sha(path):
 import hashlib;return hashlib.sha256(open(path,'rb').read()).hexdigest()[:12]

def build_info(installed,candidate,probe_path):
 """What was measured: pb version+commit before the update and under test, the probe's and the scorer's file hash."""
 return dict(installed_before=installed,candidate=candidate,probe=sha(probe_path),scorer=sha(__file__))

def pb_version(out):
 """{version,commit} from `pb version --json` output, or the reason it could not be read."""
 try:j=json.loads(out.strip().splitlines()[-1]);j=j.get('data',j);return dict(version=j['version'],commit=j['commit'])
 except Exception as e:return dict(error='unreadable: '+type(e).__name__)

def bugreport_id(out):
 """{correlation_id} from `pb bugreport --json` output (a local redacted bundle), or the failure's support reference."""
 try:j=json.loads(out.strip().splitlines()[-1]);e=j.get('error') or {}
 except Exception as x:return dict(error='unreadable: '+type(x).__name__)
 return dict(correlation_id=j['correlation_id']) if j.get('correlation_id') else dict(error=e.get('code','none'),support_reference=e.get('support_reference'))

def verdict(r):
 if r.get('info'):return r['status']+' ('+r['info']+')'  # send_big: throughput, not a stall measure
 if r['status']=='UNRESOLVED':return 'unresolved (noise %.3gs)'%r['noise_s']
 return r['status']+(' (no data in window)' if r['excess_s'] is None else '')

def render(rows,updates,build=None):
 lo,hi=min(a for a,b in updates)-LO,max(b for a,b in updates)+HI
 b=build or {};pv=lambda k:'%(version)s %(commit).12s'%b[k] if 'version' in (b.get(k) or {}) else 'unrecorded'
 print('build under test: %s (installed before: %s)  probe %s  scorer %s'%(pv('candidate'),pv('installed_before'),b.get('probe','unrecorded'),b.get('scorer','unrecorded')))
 print('window %s..%s UTC (update start-%gs .. end+%gs); verdict = worst in-window excess over pre-window median, pass <=%gs and 0 failures; above that, unresolved if noise outside the window is as high'%(hms(lo),hms(hi),LO,HI,LIMIT))
 print('%-17s %8s %9s %9s %9s %6s %7s  %s'%('feature','base_med','excess','pre_max','post_max','calls','fail/win','verdict'))
 for r in rows:
  if not r.get('events'):print('%-17s %s'%(r['feature'],'NO EVENTS  FAIL'));continue
  x=lambda k:'-' if r.get(k) is None else '%.3fs'%r[k]
  print('%-17s %8s %9s %9s %9s %6d %7s  %s'%(r['feature'],x('base_med_s'),x('excess_s'),x('pre_max_s'),x('post_max_s'),r['in_window'],'%d/%d'%(r['failures'],r['failures_in_update']),verdict(r)))
 for r in rows:
  if r.get('errors'):print('  %s failed calls (codes only): %s'%(r['feature'],', '.join('%s x%d'%kv for kv in sorted(r['errors'].items()))))

if __name__=='__main__':
 run=pathlib.Path(sys.argv[1]);u=[(iso((run/'update-start.utc').read_text()),iso((run/'update-end.utc').read_text()))]
 b=json.loads((run/'build.json').read_text()) if (run/'build.json').exists() else None
 render(score(load_events(run),u),u,b)
