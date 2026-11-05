#!/usr/bin/env python3
# noise.py QUIETRUN[:LABEL]...: the probe's intrinsic noise. Slides a synthetic WIN-second "update window" (1 s steps, from
# WARM s after the first event) across runs that had no update and reports, per feature, the p95 and max of the
# in-window excess over that run's own median, pooled over all window positions of all runs given. A LABEL (e.g.
# quiet3:pre-c231ef2b0, a run from before the fix for a stall it contains) is printed beside the run's name.
import sys,pathlib
sys.path.insert(0,str(pathlib.Path(__file__).parent));import excess
WIN,WARM=40.0,10.0
def positions(ev,feat):
 """excess of every window position: worst (value - run median) among items overlapping the window."""
 e=[x for x in ev if x['f']==feat]
 if not e:return []
 it=excess.items(e,feat in excess.STREAMS)
 if not it:return []
 v=sorted(x[2] for x in it);med=v[len(v)//2]
 t0=min(x['t'] for x in ev if x['f'] in excess.FEATS)+WARM;t1=max(x['t'] for x in ev if x['f'] in excess.FEATS)
 out=[];w=t0
 while w+WIN<=t1:
  out.append(max(0,max((val-med for s,t,val in it if t>=w and s<=w+WIN),default=0)));w+=1
 return out
def pct(v,q):v=sorted(v);return v[min(len(v)-1,int(q*len(v)))]
if __name__=='__main__':
 runs=[a.partition(':')[::2] for a in sys.argv[1:]];names=[pathlib.Path(r).name+(' ('+l+')' if l else '') for r,l in runs]
 evs=[excess.load_events(pathlib.Path(r)) for r,l in runs]
 print('noise floor: %gs windows slid in 1 s steps (first %gs of each run skipped) over %d runs: %s'%(WIN,WARM,len(evs),', '.join(names)))
 print('%-17s %8s %8s %8s   per run max'%('feature','p50','p95','max'))
 for f in excess.FEATS:
  per=[positions(ev,f) for ev in evs];allp=[x for p in per for x in p]
  if not allp:print('%-17s no data'%f);continue
  print('%-17s %7.3fs %7.3fs %7.3fs   %s'%(f,pct(allp,.5),pct(allp,.95),max(allp),' '.join('%.2f'%max(p) if p else '-' for p in per)))
