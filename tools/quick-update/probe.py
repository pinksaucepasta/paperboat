#!/usr/bin/env python3
# Per-feature probes during a pb update. Writes OUT/events.jsonl: one line per probe
# event {f: feature, t: unix time, ok: bool, ms: latency}. No terminal/file content stored.
import subprocess,threading,time,json,sys,os,urllib.request,ssl
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
lock=threading.Lock();f=open(os.path.join(out,'events.jsonl'),'a')
def ev(feat,ok,ms=None,**k):
 with lock:f.write(json.dumps(dict(f=feat,t=time.time(),ok=ok,ms=ms,**k))+'\n');f.flush()
stop=lambda:os.path.exists(stop_file)
def loop(feat,cmd,every,timeout=30):
 while not stop():
  s=time.time()
  try:ok=subprocess.run(cmd,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=timeout).returncode==0
  except subprocess.TimeoutExpired:ok=False
  ev(feat,ok,int((time.time()-s)*1000));time.sleep(max(0,every-(time.time()-s)))
def stream(feat,cmd):
 while not stop():
  p=subprocess.Popen(cmd,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
  ev(feat+'_start',True)
  for line in p.stdout:
   ev(feat,True)
   if stop():break
  p.kill();rc=p.wait();ev(feat+'_end',rc==0,rc=rc)
def http(feat,url,every):
 ctx=ssl.create_default_context()
 while not stop():
  s=time.time()
  try:
   with urllib.request.urlopen(url,timeout=5,context=ctx) as r:ok=r.status==200;r.read(64)
  except Exception as e:ok=False
  ev(feat,ok,int((time.time()-s)*1000));time.sleep(max(0,every-(time.time()-s)))
def stdin_echo(feat,size,every):
 # stdin in must equal stdout out (2026-11-05: pb exec dropped stdin entirely after 407955aa).
 import hashlib
 while not stop():
  data=os.urandom(size); s0=time.time()
  try:
   r=subprocess.run([pb,'exec',tgt,'--','cat'],input=data,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=60)
   ok=r.returncode==0 and hashlib.sha256(r.stdout).digest()==hashlib.sha256(data).digest()
  except subprocess.TimeoutExpired:ok=False
  ev(feat,ok,int((time.time()-s0)*1000));time.sleep(max(0,every-(time.time()-s0)))
def send(path):
 # pb send reports no progress, so: a small file back to back; each call's latency is the measure (excess.py).
 while not stop():
  s=time.time()
  try:ok=subprocess.run([pb,'send',path,'--to',tgt,'--json'],capture_output=True,text=True,timeout=60).returncode==0
  except subprocess.TimeoutExpired:ok=False
  ev('send',ok,int((time.time()-s)*1000));time.sleep(0.2)
ticker="i=0; while [ $i -lt 100000 ]; do date +%s.%N; i=$((i+1)); sleep 0.1; done"
T=[threading.Thread(target=loop,args=('exec_short',[pb,'exec',tgt,'--','true'],0.2)),
   threading.Thread(target=stream,args=('exec_long',[pb,'exec',tgt,'--','sh','-c',ticker])),
   threading.Thread(target=stream,args=('ssh_stream',[pb,'ssh',tgt,'--','-o','BatchMode=yes','-o','StrictHostKeyChecking=accept-new',ticker])),
   threading.Thread(target=loop,args=('ssh_connect',[pb,'ssh',tgt,'--','-o','BatchMode=yes','-o','StrictHostKeyChecking=accept-new','true'],1.0)),
   threading.Thread(target=stdin_echo,args=('exec_stdin',64,1.0)),
   threading.Thread(target=stdin_echo,args=('exec_stdin_large',1<<20,10.0)),
   threading.Thread(target=send,args=(os.path.join(os.path.dirname(out),'big.bin'),))]
if preview!='-':T.append(threading.Thread(target=http,args=('preview',preview,0.2)))
if tunnel!='-':T.append(threading.Thread(target=http,args=('tunnel',tunnel,0.2)))
for t in T+[threading.Thread(target=_refresh)]:t.daemon=True;t.start()
while not stop():time.sleep(0.5)
time.sleep(2);ev('probe_done',True)
