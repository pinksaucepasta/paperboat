#!/usr/bin/env python3
# setup.py BASELINE: a fresh paired target + controller on one update-test host, both on BASELINE.
# Env: QV_VM (ssh alias of a VM, or "local" = this machine is the target, e.g. a CI runner), QV_PREFIX (throwaway
# prefix, e.g. lanegap-vm1-), QV_STATE (a private scratch dir), QV_SERVER + QV_WEBTOKEN (see mint-token.py).
# The target is the host user's pb install; the controller is a systemd container (machine QV_PREFIX+ctl).
# Enrollment tokens go file to file (mint-token.py -> 0600 file -> target/controller -> pair), never printed.
import subprocess,sys,pathlib,shlex,os
os.umask(0o077)
base=pathlib.Path(sys.argv[1]).resolve();here=pathlib.Path(__file__).resolve().parent;smoke=here.parent/'device-smoke'
H=os.environ['QV_VM'];P=os.environ['QV_PREFIX'];TG=P+'target';CT=P+'ctl';ST=pathlib.Path(os.environ['QV_STATE']);ST.mkdir(parents=True,exist_ok=True)
LOCAL=H=='local';home=os.path.expanduser('~')
def sh(cmd,timeout=600,check=True):
 r=subprocess.run(cmd,shell=True,capture_output=True,text=True,timeout=timeout)
 (ST/'setup.log').open('a').write('$ '+cmd[:200]+'\n'+r.stdout[-3000:]+r.stderr[-3000:]+'\n')
 if check and r.returncode:raise SystemExit('failed: '+cmd[:120])
 return r.stdout
R=lambda c,**k:sh(('bash -c '+shlex.quote(c)) if LOCAL else ('ssh -o BatchMode=yes -o ConnectTimeout=20 '+H+' '+shlex.quote(c)),**k)
def put(src,dst):
 if LOCAL and os.path.exists(f'{home}/{dst}') and os.path.samefile(src,f'{home}/{dst}'):return  # already in place (run.sh builds into ~/gap5)
 sh(f'cp -r {src} {home}/{dst}' if LOCAL else f'scp -qr {src} {H}:{dst}')
U=f'sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {CT}'
def tok(name,dest):
 sh(f"python3 -I {here}/mint-token.py {name} {ST}/t.tok",timeout=120);put(f'{ST}/t.tok',f'gap5/{dest}');os.unlink(ST/'t.tok')
R('mkdir -p ~/gap5/smoke')
put(str(base),'gap5/baseline');put(f'{here}/measure_held.py',f'gap5/');put(f'{here}/probe.py','gap5/');put(f'{here}/mksession.py','gap5/')
sh(f'rsync -a '+('' if LOCAL else '-e "ssh -o BatchMode=yes" ')+f'--exclude=results --exclude=__pycache__ {smoke}/ '+(f'{home}/gap5/smoke/' if LOCAL else f'{H}:gap5/smoke/'));put(f'{here}/measure_held.py','gap5/smoke/')
R('chmod 755 ~/gap5/baseline')
R('cd ~/gap5 && ./baseline install --json > install.raw 2>&1',timeout=600)
tok(TG,'tok')
R(f'chmod 600 ~/gap5/tok; ~/.local/bin/pb pair --name {TG} --enrollment-token-file ~/gap5/tok --json > ~/gap5/pair.raw 2>&1; rc=$?; rm -f ~/gap5/tok; exit $rc',timeout=300)
R(f'sudo docker rm -f {CT} >/dev/null 2>&1; sudo docker run -d --name {CT} --hostname {CT} --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock gap5-systemd && sleep 8 && sudo docker exec {CT} loginctl enable-linger pbt && sudo docker exec {CT} systemctl start user@1000.service')
R(f'sudo docker cp ~/gap5/baseline {CT}:/home/pbt/pb && sudo docker cp ~/gap5/smoke {CT}:/tmp/smoke && sudo docker exec {CT} chown -R pbt:pbt /home/pbt/pb /tmp/smoke && {U} ./pb install --json > ~/gap5/ctl-install.raw 2>&1',timeout=600)
tok(CT,'ctok')
R(f'sudo docker cp ~/gap5/ctok {CT}:/home/pbt/tok; rm -f ~/gap5/ctok; sudo docker exec {CT} chown pbt /home/pbt/tok; {U} /home/pbt/.local/bin/pb pair --name {CT} --enrollment-token-file /home/pbt/tok --json > ~/gap5/ctl-pair.raw 2>&1; rc=$?; sudo docker exec {CT} rm -f /home/pbt/tok; exit $rc',timeout=300)
print(R(f'~/.local/bin/pb version --json | head -c 300; ~/.local/bin/pb doctor --json >/dev/null && echo target-doctor-ok; {U} /home/pbt/.local/bin/pb doctor --json >/dev/null && echo ctl-doctor-ok',check=False))
