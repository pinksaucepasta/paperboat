import pty, os, time, select, signal, sys, re
PB=os.path.expanduser('~/.local/bin/pb'); T=sys.argv[1]; DELAY=float(sys.argv[2])
def spawn(args):
    pid, fd = pty.fork()
    if pid == 0:
        os.environ['TERM']='xterm-256color'; os.execv(PB,[PB,'connect',T,'--no-customization']+args)
    return pid, fd
SESS=len(sys.argv)>3
if SESS:
    name='rx24ee%d'%int(time.time()%100000)
    pid0,fd0=spawn(['new','--name',name]); time.sleep(10); os.kill(pid0,signal.SIGKILL); os.waitpid(pid0,0); os.close(fd0); time.sleep(2)
    pid, fd = spawn(['--session',name])
else:
    pid, fd = spawn([])
def drain(secs):
    out=b''; end=time.time()+secs
    while time.time()<end:
        r,_,_=select.select([fd],[],[],0.2)
        if r:
            try: d=os.read(fd,65536)
            except OSError: break
            if not d: break
            out+=d
    return out
o=drain(DELAY)
os.write(fd,b'echo EARLY_%d_OK\r' % int(DELAY*10))
o+=drain(25)
txt=o.decode('utf-8','ignore')
ran = txt.count('EARLY_%d_OK' % int(DELAY*10)) >= 2
print('delay',DELAY,'command ran:',ran)
os.write(fd,b'exit\r'); drain(2)
try: os.kill(pid,signal.SIGKILL)
except: pass
