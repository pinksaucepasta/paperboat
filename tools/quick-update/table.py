#!/usr/bin/env python3
# table.py RUNDIR: the per-feature table of one features.py run (max pause, failures), then excess.py's view.
import json,sys,subprocess,pathlib
run=pathlib.Path(sys.argv[1]);s=json.load(open(run/'summary.json'))
print(f"run {s['run']}  update exit {s['update_exit']}  update {s['update_start']}..{s['update_end']} UTC")
print('%-17s %9s %8s %9s %8s %9s'%('feature','max_pause','events','failures','fail_in_upd','restarts'))
for r in s['rows']:
 if not r.get('events'):print('%-17s %9s %8d'%(r['feature'],'NO EVENTS',0));continue
 print('%-17s %8ss %8d %9d %8d %9d'%(r['feature'],r.get('worst_gap_s'),r['events'],r.get('failures',0),r.get('failures_in_update',0),r.get('restarts',0)))
print('\nexcess over pre-update median (excess.py):')
sys.stdout.flush();subprocess.run([sys.executable,str(pathlib.Path(__file__).with_name('excess.py')),str(run)])
