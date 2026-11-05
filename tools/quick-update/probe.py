#!/usr/bin/env python3
# Per-feature probes during a pb update. Writes OUT/events.jsonl: one line per probe
# event {f: feature, t: unix time, ok: bool, ms: latency}; a failed call adds rc (exit status) and err (a code).
# No terminal/file content or pb message text stored.
import subprocess,threading,time,json,sys,os,urllib.request,urllib.error,ssl,tempfile
import socket
# The controller container's resolver loses about 1 in 300 UDP queries (each then waits its 5 s retry) and
# drops AAAA queries outright (prefix1, quiet1, quiet2). Like a browser, the HTTP probes resolve each host once
# (IPv4, full address list so both edges stay reachable) and refresh it in the background every 30 s, so a
# request never waits on DNS. Remove when the container gets a working resolver.
_gai=socket.getaddrinfo;_dns={}
def _cached(host,port,family=0,*a,**k):
 key=(host,port)
 if key not in _dns:_dns[key]=_gai(host,port,socket.AF_INET,*a,**k)
 return _dns[key]
def _refresh():
 while True:
  time.sleep(30)
  for (h,p) in list(_dns):
   try:_dns[(h,p)]=_gai(h,p,socket.AF_INET,0,socket.SOCK_STREAM)
   except OSError:pass
socket.getaddrinfo=_cached
tgt,preview,tunnel,out,stop_file=sys.argv[1:6]
pb=os.path.expanduser('~/.local/bin/pb');os.makedirs(out,exist_ok=True)
# events.jsonl is uploaded as a public CI artifact: a failed call records only its exit status and a code (never pb's
# message, which can name machines or files). PROBE_STDERR=1 (private runs only) also keeps pb's last 2 KB of stderr
# per failed call in failures.jsonl, which is never uploaded.
KEEP=os.environ.get('PROBE_STDERR')=='1'
lock=threading.Lock();f=open(os.path.join(out,'events.jsonl'),'a');ff=open(os.path.join(out,'failures.jsonl'),'a') if KEEP else None
def ev(feat,ok,ms=None,**k):
 with lock:f.write(json.dumps(dict(f=feat,t=time.time(),ok=ok,ms=ms,**k))+'\n');f.flush()
def fail(feat,detail):
 if ff:
  with lock:ff.write(json.dumps(dict(f=feat,t=time.time(),stderr=detail))+'\n');ff.flush()
tail=lambda b:(b or b'')[-2048:].decode('utf-8','replace')
stop=lambda:os.path.exists(stop_file)
def send_error(stdout):
 # A failed send's JSON error code, stage and category only: its message and target can name files or machines.
 try:e=json.loads(stdout.decode('utf-8','replace').strip().splitlines()[-1]).get('error') or {}
 except (ValueError,IndexError,AttributeError):return 'unparsed'
 return ':'.join(str(e.get(k,'')) for k in ('code','stage','category'))
def call(feat,cmd,timeout,input=None,stdout=subprocess.DEVNULL,verify=None,code=None,**extra):
 """One pb call, recorded: ok and latency; a failed call also its exit status (rc, None on timeout) and a code."""
 s=time.time()
 try:r=subprocess.run(cmd,input=input,stdout=stdout,stderr=subprocess.PIPE,timeout=timeout)
 except subprocess.TimeoutExpired as e:r=subprocess.CompletedProcess(cmd,None,b'',e.stderr)
 ok=r.returncode==0 and (verify is None or verify(r));k={}
 if not ok:
  k=dict(rc=r.returncode,err='timeout' if r.returncode is None else 'mismatch' if r.returncode==0 else code(r) if code else 'exit%d'%r.returncode)
  fail(feat,tail(r.stderr))
 ev(feat,ok,int((time.time()-s)*1000),**extra,**k)
def loop(feat,cmd,every,timeout=30):
 while not stop():
  s=time.time();call(feat,cmd,timeout);time.sleep(max(0,every-(time.time()-s)))
def stream(feat,cmd):
 while not stop():
  errf=tempfile.TemporaryFile()
  p=subprocess.Popen(cmd,stdout=subprocess.PIPE,stderr=errf,text=True)
  ev(feat+'_start',True)
  for line in p.stdout:
   ev(feat,True)
   if stop():break
  p.kill();rc=p.wait()
  if rc!=0 and not stop():  # ended by itself, not by our kill at the end of the run
   errf.seek(0,2);errf.seek(max(0,errf.tell()-2048));fail(feat+'_end',tail(errf.read()))
  errf.close();ev(feat+'_end',rc==0,rc=rc)
def http(feat,url,every):
 ctx=ssl.create_default_context()
 while not stop():
  s=time.time();err=None
  try:
   with urllib.request.urlopen(url,timeout=5,context=ctx) as r:
    if r.status!=200:err='http%d'%r.status
    r.read(64)
  except Exception as e:err='http%d'%e.code if isinstance(e,urllib.error.HTTPError) else type(e).__name__;fail(feat,str(e)[:2048])
  ev(feat,err is None,int((time.time()-s)*1000),**({'err':err} if err else {}));time.sleep(max(0,every-(time.time()-s)))
def stdin_echo(feat,size,every):
 # stdin in must equal stdout out (2026-11-05: pb exec dropped stdin entirely after 407955aa).
 import hashlib
 while not stop():
  data=os.urandom(size);s=time.time()
  call(feat,[pb,'exec',tgt,'--','cat'],60,input=data,stdout=subprocess.PIPE,verify=lambda r:hashlib.sha256(r.stdout).digest()==hashlib.sha256(data).digest())
  time.sleep(max(0,every-(time.time()-s)))
def send(feat,path,every,timeout):
 # pb send prints only its final result, so a stall inside a send is not visible apart from the send's total time. `send`
 # is a 64 KiB file every second: its own latency is the pause (excess.py). `send_big` is 20 MB every 10 s: the
 # throughput check, not a stall measure.
 size=os.path.getsize(path)
 while not stop():
  s=time.time();call(feat,[pb,'send',path,'--to',tgt,'--json'],timeout,stdout=subprocess.PIPE,code=lambda r:send_error(r.stdout),bytes=size)
  time.sleep(max(0,every-(time.time()-s)))
ticker="i=0; while [ $i -lt 100000 ]; do date +%s.%N; i=$((i+1)); sleep 0.1; done"
T=[threading.Thread(target=loop,args=('exec_short',[pb,'exec',tgt,'--','true'],0.2)),
   threading.Thread(target=stream,args=('exec_long',[pb,'exec',tgt,'--','sh','-c',ticker])),
   threading.Thread(target=stream,args=('ssh_stream',[pb,'ssh',tgt,'--','-o','BatchMode=yes','-o','StrictHostKeyChecking=accept-new',ticker])),
   threading.Thread(target=loop,args=('ssh_connect',[pb,'ssh',tgt,'--','-o','BatchMode=yes','-o','StrictHostKeyChecking=accept-new','true'],1.0)),
   threading.Thread(target=stdin_echo,args=('exec_stdin',64,1.0)),
   threading.Thread(target=stdin_echo,args=('exec_stdin_large',1<<20,10.0)),
   threading.Thread(target=send,args=('send',os.path.join(os.path.dirname(out),'small.bin'),1.0,60)),
   threading.Thread(target=send,args=('send_big',os.path.join(os.path.dirname(out),'big.bin'),10.0,120))]
if preview!='-':T.append(threading.Thread(target=http,args=('preview',preview,0.2)))
if tunnel!='-':T.append(threading.Thread(target=http,args=('tunnel',tunnel,0.2)))
for t in T+[threading.Thread(target=_refresh)]:t.daemon=True;t.start()
while not stop():time.sleep(0.5)
time.sleep(2);ev('probe_done',True)
