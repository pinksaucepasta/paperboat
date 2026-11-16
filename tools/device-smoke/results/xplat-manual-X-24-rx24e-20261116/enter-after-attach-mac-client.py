import pty, os, time, select, signal, sys
PB='/Users/admin/.local/bin/pb'; T=sys.argv[1]; GAP=float(sys.argv[2]); N=int(sys.argv[3])
def spawn(args):
    pid, fd = pty.fork()
    if pid == 0:
        os.environ['TERM']='xterm-256color'; os.execv(PB,[PB,'connect',T,'--no-customization']+args)
    return pid, fd
def drain(fd,secs,until=None):
    out=b''; end=time.time()+secs
    while time.time()<end:
        r,_,_=select.select([fd],[],[],0.1)
        if r:
            try: d=os.read(fd,65536)
            except OSError: break
            if not d: break
            out+=d
            if until and until in out: break
    return out
name='rx24ea%d'%int(time.time()%100000)
pid0,fd0=spawn(['new','--name',name]); drain(fd0,20,b'>'); time.sleep(1); os.kill(pid0,signal.SIGKILL); os.waitpid(pid0,0); os.close(fd0); time.sleep(2)
ok=0
for i in range(N):
    pid,fd=spawn(['--session',name]); o=drain(fd,30,b'>')
    t_attach=time.time()
    time.sleep(GAP)
    tag='AA%d_%d'%(int(GAP*100),i)
    for ch in ('echo %s'%tag):
        os.write(fd,ch.encode()); time.sleep(0.01)
    time.sleep(0.05); os.write(fd,b'\r')
    o=drain(fd,6)
    ran=o.decode('utf-8','ignore').count(tag)>=2
    ok+=ran
    print('gap',GAP,'try',i,'ran',ran, '' if ran else repr(o.decode('utf-8','ignore')[-400:]))
    os.kill(pid,signal.SIGKILL); os.waitpid(pid,0); os.close(fd); time.sleep(2)
print('result gap',GAP,ok,'of',N)
