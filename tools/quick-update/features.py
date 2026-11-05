#!/usr/bin/env python3
# Feature run on one update-test target (a VM or a CI runner): probes every feature from the controller container while the
# target runs pb update. Usage: features.py RUNDIR CANDIDATE|- PREVIEW_URL TUNNEL_URL
# CANDIDATE '-' runs the same windows with no update (quiet baseline).
import subprocess,sys,pathlib,json,os,time,datetime
os.umask(0o077)
run=pathlib.Path(sys.argv[1]);cand=sys.argv[2];pv,tn=sys.argv[3],sys.argv[4]
run.mkdir(parents=True,exist_ok=False)
H=os.environ['QV_VM'];CT=os.environ['QV_CTL'];TG=os.environ['QV_TARGET'];name=run.name
# QV_VM=local: the target is this machine (a GitHub runner); every remote command runs through bash instead of ssh.
LOCAL=H=='local'
SSHX=lambda c:['bash','-c',c] if LOCAL else ['ssh',H,c]
PUT=lambda src,dst:['cp',src,os.path.expanduser('~/'+dst)] if LOCAL else ['scp','-q',src,f'{H}:{dst}']
SRV=os.environ['QV_SERVER']
U=f'sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {CT}'
def R(c,timeout=300,check=True):
 r=subprocess.run(SSHX(c),capture_output=True,text=True,timeout=timeout)
 (run/'steps.log').open('a').write('$ '+c[:160]+'\n'+r.stdout[-2000:]+r.stderr[-2000:]+'\n')
 if check and r.returncode:raise SystemExit('step failed: '+c[:100]+' :: '+(r.stderr or r.stdout or '').strip()[-500:])
 return r.stdout
now=lambda:datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='milliseconds')
R(f'mkdir -p ~/gap5/{name}')
if cand!='-':subprocess.run(PUT(cand,f'gap5/{name}/candidate'),check=True)
subprocess.run(PUT(str(pathlib.Path(__file__).with_name('probe.py')),'gap5/probe.py'),check=True)
R(f'chmod 755 ~/gap5/{name}/candidate 2>/dev/null; sudo docker cp ~/gap5/probe.py {CT}:/tmp/probe.py && sudo docker exec {CT} chown pbt /tmp/probe.py')
R(f"{U} sh -c 'mkdir -p /home/pbt/{name} && head -c 65536 /dev/urandom > /home/pbt/{name}/big.bin'")
R(f"{U} bash -c 'nohup python3 /tmp/probe.py {TG} {pv} {tn} /home/pbt/{name}/p /home/pbt/{name}/stop > /home/pbt/{name}/probe.out 2>&1 &'")
# Held browser terminal: a guest joins one owner session by code through the console's own page (WASM browser
# terminal) from the controller host, types a 200 ms echo ticker and records each new marker (web/webheld.mjs).
NOWEB=os.environ.get('NO_WEB')=='1';webp=None
if not NOWEB:
 W=pathlib.Path(__file__).resolve().parent;TOK=os.environ['QV_WEBTOKEN'];wname='smoke-web-'+name
 subprocess.run(PUT(str(W/'mksession.py'),'gap5/mksession.py'),check=True)
 sid=R(f'sudo docker cp ~/gap5/mksession.py {CT}:/tmp/mksession.py && sudo docker exec {CT} chown pbt /tmp/mksession.py && {U} python3 /tmp/mksession.py {TG} {wname}').strip().splitlines()[-1]
 (run/'web-session').write_text(sid)
 subprocess.run([sys.executable,str(W/'code.py'),TOK,SRV,sid,str(run/'web.code')],check=True,stdout=subprocess.DEVNULL)
 wenv={**os.environ,'SESSION':sid,'CODEFILE':str(run/'web.code'),'OUT':str(run/'web-events.jsonl'),'STOP':str(run/'web.stop'),'MAX_S':'1500'}  # PW_MODULE, PW_CHROMIUM, PB_CONSOLE_ORIGIN come from the environment
 webp=subprocess.Popen(['node',str(W/'webheld.mjs')],env=wenv,stdout=(run/'web.out').open('w'),stderr=subprocess.STDOUT)
 for _ in range(150):
  if (run/'web-events.jsonl').exists() and '"term_web"' in (run/'web-events.jsonl').read_text():break
  if webp.poll() is not None:raise SystemExit('browser terminal exited before its first marker')
  time.sleep(1)
 else:raise SystemExit('browser terminal showed no marker')

# Held CLI terminal: pb connect from the controller, a marker every 100 ms read back (measure_held.py).
R(f"{U} bash -c 'nohup python3 /tmp/smoke/measure_held.py {TG} /home/pbt/{name}/held --count 2000 > /home/pbt/{name}/held.out 2>&1 &'")
for _ in range(120):
 if subprocess.run(SSHX(f'{U} test -f /home/pbt/{name}/held/ready'),capture_output=True).returncode==0:break
 time.sleep(1)
else:raise SystemExit('held terminal not ready')
# Probes start >30 s before the update: the analysis window opens 30 s before it, and probe
# start-up (the controller resolver's first lookup takes 5 s) must stay outside it (prefix1).
time.sleep(45)
t0=now();(run/'update-start.utc').write_text(t0)
if cand=='-':
 time.sleep(60);r=subprocess.CompletedProcess([],0,stdout='none')
else:
 r=subprocess.run(SSHX(f'cd ~/gap5/{name} && ~/.local/bin/pb update --from ~/gap5/{name}/candidate --json > update.raw 2>&1; echo $?'),capture_output=True,text=True,timeout=900)
t1=now();(run/'update-end.utc').write_text(t1);(run/'update-exit').write_text(r.stdout.strip())
# Session owners right after the update; the previous generation's retirement is timed by owner-watch below.
R(f"systemctl list-units --no-legend 'paperboat-session-owner-*' | awk '{{print $1}}' > ~/gap5/{name}/owners-after.txt; (for i in $(seq 1 600); do echo \"$(date -u +%H:%M:%S.%3N) $(systemctl list-units --no-legend 'paperboat-session-owner-*' | awk '{{print $1}}' | tr '\\n' ' ')\"; sleep 1; done > ~/gap5/{name}/owner-watch.txt 2>&1 < /dev/null &)",check=False)
# Keep each window's diagnostics before the probes' traffic rotates them out (feat7 lost them).
R(f"cd ~/gap5/{name} && sudo tar czf target-root-diag-update.tgz /root/.local/state/paperboat/diagnostics /var/lib/paperboat-updated-u*/diagnostics /var/lib/paperboat-machineguard/diagnostics 2>/dev/null; sudo chown $(id -un) target-root-diag-update.tgz; tar czf target-diag-update.tgz -C ~/.local/state/paperboat diagnostics; {U} tar czf /home/pbt/{name}/ctl-diag-update.tgz -C /home/pbt/.local/state/paperboat diagnostics; sudo docker cp {CT}:/home/pbt/{name}/ctl-diag-update.tgz ~/gap5/{name}/ctl-diag-update.tgz",check=False)
time.sleep(60)  # recovery window
R(f"cd ~/gap5/{name} && sudo tar czf target-root-diag-recovery.tgz /root/.local/state/paperboat/diagnostics /var/lib/paperboat-updated-u*/diagnostics /var/lib/paperboat-machineguard/diagnostics 2>/dev/null; sudo chown $(id -un) target-root-diag-recovery.tgz; tar czf target-diag-recovery.tgz -C ~/.local/state/paperboat diagnostics; {U} tar czf /home/pbt/{name}/ctl-diag-recovery.tgz -C /home/pbt/.local/state/paperboat diagnostics; sudo docker cp {CT}:/home/pbt/{name}/ctl-diag-recovery.tgz ~/gap5/{name}/ctl-diag-recovery.tgz",check=False)
R(f'{U} touch /home/pbt/{name}/stop');(run/'web.stop').touch();time.sleep(8)
try:webp and webp.wait(timeout=60)
except subprocess.TimeoutExpired:webp.kill()
(run/'web.code').unlink(missing_ok=True)
if not NOWEB:R(f"{U} python3 -c \"import sys;sys.path.insert(0,'/tmp/smoke');from session_cleanup import cleanup;cleanup('/home/pbt/.local/bin/pb','{TG}','{wname}')\"",check=False)
# The probe kills its clients; the remote tickers are its own and must not outlive the run.
R("pkill -u $(id -un) -f '^(ba)?sh -c i=0; while' || true",check=False)
# exec_stdin clients killed on their timeout leave the remote cat waiting on stdin (dadape 2026-11-05); count, then remove
(run/'orphans.txt').write_text(R("n=0; for p in $(pgrep -u $(id -un) -f __runtime-session-owner); do c=$(pgrep -P $p -x cat | wc -l); n=$((n+c)); pkill -P $p -x cat; done; echo $n; systemctl list-units --no-legend 'paperboat-session-owner-*' | wc -l",check=False))
# The held terminal stops after its 2000 markers; its marker receive times become term_cli rows.
for _ in range(300):
 if subprocess.run(SSHX(f'{U} test -f /home/pbt/{name}/held/result.json'),capture_output=True).returncode==0:break
 time.sleep(1)
R(f'{U} cat /home/pbt/{name}/held/result.json > ~/gap5/{name}/held-result.json; {U} cat /home/pbt/{name}/held/markers.json > ~/gap5/{name}/held-markers.json',check=False)
R(f'{U} cat /home/pbt/{name}/p/events.jsonl > ~/gap5/{name}/events.jsonl; {U} rm -f /home/pbt/{name}/big.bin',check=False)
subprocess.run(['rsync','-a',(os.path.expanduser(f'~/gap5/{name}/') if LOCAL else f'{H}:gap5/{name}/'),str(run)+'/','--exclude=candidate'],timeout=300)
sys.path.insert(0,os.path.dirname(os.path.abspath(__file__)));import excess  # the one scoring module (windows, excess, verdicts)
ts=excess.iso
ev=excess.load_events(run)
rows=excess.score(ev,[(ts(t0),ts(t1))])
s=dict(run=name,update_exit=r.stdout.strip(),update_start=t0[11:23],update_end=t1[11:23],rows=rows,updates=[(t0,t1)])
(run/'summary.json').write_text(json.dumps(s,indent=1));print(json.dumps(s))
