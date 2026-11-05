#!/usr/bin/env python3
# table.py RUNDIR: the per-feature verdict table of one features.py run (excess.py's metrics, from summary.json).
import json,sys,pathlib
sys.path.insert(0,str(pathlib.Path(__file__).parent));import excess
run=pathlib.Path(sys.argv[1]);s=json.load(open(run/'summary.json'))
print(f"run {s['run']}  update exit {s['update_exit']}  update {s['update_start']}..{s['update_end']} UTC")
excess.render(s['rows'],[(excess.iso(a),excess.iso(b)) for a,b in s['updates']])
