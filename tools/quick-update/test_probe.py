#!/usr/bin/env python3
# python3 test_probe.py: probe.py against a fake `pb` that always fails with a message naming a "machine".
# events.jsonl (uploaded as a public artifact) may hold exit status and codes, never that text; the stderr tail is kept
# only in failures.jsonl, and only when PROBE_STDERR=1 (private runs).
import json,os,pathlib,subprocess,sys,tempfile,time,unittest
HERE=pathlib.Path(__file__).resolve().parent
FAKE='''#!/bin/sh
if [ "$1" = send ]; then
  echo '{"ok":false,"error":{"code":"transfer_failed","stage":"upload","category":"network","message":"cannot reach SECRETHOST"}}'
  echo "SECRETHOST unreachable" >&2; exit 1
fi
echo "SECRETHOST: boom" >&2; sleep 0.05; exit 3
'''
def start(private):
 d=pathlib.Path(tempfile.mkdtemp());(d/'home/.local/bin').mkdir(parents=True)
 pb=d/'home/.local/bin/pb';pb.write_text(FAKE);pb.chmod(0o755)
 (d/'small.bin').write_bytes(b'x'*64);(d/'big.bin').write_bytes(b'x'*64)
 env=dict(os.environ,HOME=str(d/'home'));env.pop('PROBE_STDERR',None)
 if private:env['PROBE_STDERR']='1'
 return d,subprocess.Popen([sys.executable,'-I',str(HERE/'probe.py'),'tgt','-','-',str(d/'p'),str(d/'stop')],env=env)
def events(d,name='events.jsonl'):
 p=d/'p'/name;return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []
class T(unittest.TestCase):
 @classmethod
 def setUpClass(c):
  (pub,a),(prv,b)=start(False),start(True);time.sleep(2.5)
  for d in (pub,prv):(d/'stop').touch()
  a.wait(timeout=30);b.wait(timeout=30);c.pub,c.prv=pub,prv
 def failed(self,d,f):return [e for e in events(d) if e['f']==f and not e['ok']]
 def test_failed_calls_carry_exit_status_and_code(self):
  for d in (self.pub,self.prv):
   for f in ('exec_short','ssh_connect','exec_stdin','exec_stdin_large'):
    e=self.failed(d,f);self.assertTrue(e,f);self.assertEqual((e[0]['rc'],e[0]['err']),(3,'exit3'),f)
   e=self.failed(d,'send');self.assertTrue(e);self.assertEqual((e[0]['rc'],e[0]['err']),(1,'transfer_failed:upload:network'))
   e=self.failed(d,'send_big');self.assertTrue(e);self.assertEqual(e[0]['bytes'],64)
   e=self.failed(d,'exec_long_end');self.assertTrue(e);self.assertEqual(e[0]['rc'],3)
 def test_the_public_events_never_hold_pbs_text(self):
  for d in (self.pub,self.prv):self.assertNotIn('SECRETHOST',(d/'p'/'events.jsonl').read_text())
  self.assertFalse((self.pub/'p'/'failures.jsonl').exists())
 def test_private_runs_keep_the_stderr_tail_only(self):
  rows=events(self.prv,'failures.jsonl');self.assertTrue(rows)
  for f in ('exec_short','send','exec_long_end'):self.assertIn('SECRETHOST',[r for r in rows if r['f']==f][0]['stderr'])
  tails=[r['stderr'].split(' | stdout: ')[0] for r in rows]  # a failed call's stdout rides after that marker
  self.assertTrue(all(len(t)<=2048 and 'cannot reach' not in t for t in tails))  # the stderr tail is not pb's stdout
if __name__=='__main__':unittest.main()
