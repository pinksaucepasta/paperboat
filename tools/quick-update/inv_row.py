#!/usr/bin/env python3
# inv-row.py LABEL FILE: one table row for the no-leftovers check (install_inventory_check.sh output in FILE).
# PASS only when the check ran and "remaining" is empty; a check that could not run is a FAIL, never a skip.
import json,sys,pathlib
label,f=sys.argv[1],pathlib.Path(sys.argv[2]);raw=f.read_text(errors='replace') if f.exists() else ''
row='%-30s'%('no_leftovers_'+label)
if 'service requires install, commit, repair' in raw:  # an older pb: __runtime-service has no inventory command yet
 print(row+'SKIPPED this build has no `__runtime-service inventory --check` (install_inventory_check.sh needs a build with it); not a leftover');sys.exit(0)
try:
 lines=[l for l in raw.splitlines() if l.strip().startswith('{')]
 if not lines:raise ValueError('')
 d=json.loads(lines[0])
 rem=d.get('remaining',d.get('data',{}).get('remaining')) if isinstance(d,dict) else None
 if rem is None:raise ValueError('no "remaining" in the check output: '+raw.strip()[:200])
 # The controller container shares the host's process table, so the host-wide check also sees the controller's own
 # processes (a second line {"container_pids":[..]} from `docker top`); they are the controller's, not the target's.
 cpids={str(x) for x in (json.loads(lines[1]).get('container_pids',[]) if len(lines)>1 else [])}
 own=[r for r in rem if r.startswith('process ') and r.split()[1] in cpids];rem=[r for r in rem if r not in own]
 note=' (ignored %d processes of the controller container)'%len(own) if own else ''
 print(row+note+' '+('PASS' if not rem else 'FAIL remaining=%d: %s'%(len(rem),json.dumps(rem)[:600])));sys.exit(1 if rem else 0)
except (StopIteration,ValueError) as e:
 print(row+'FAIL check did not run: '+(str(e) or raw.strip()[:200]).replace('\n',' ')[:240]);sys.exit(1)
