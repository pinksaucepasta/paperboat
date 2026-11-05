#!/usr/bin/env python3
# python3 excess_test.py: synthetic events through excess.score (update at t=100..130, window 98..140).
import sys,pathlib,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parent));import excess
U=[(100.0,130.0)]
def calls(f,ends,ms,ok=True):return [dict(f=f,t=t,ok=ok,ms=int(ms*1000)) for t in ends]
def ticks(f,times):return [dict(f=f,t=t,ok=True,ms=None) for t in times]
def row(ev,f):return {r['feature']:r for r in excess.score(ev,U)}[f]
frange=lambda a,b,s:[a+i*s for i in range(int((b-a)/s)+1)]

class T(unittest.TestCase):
 def test_pre_window_slow_call_excluded(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[60.0],3.0)  # slow call 40 s before the update
  r=row(ev,'exec_short')
  self.assertLess(r['excess_s'],0.3);self.assertAlmostEqual(r['pre_max_s'],1.9,1);self.assertTrue(r['ok'])
 def test_post_window_slow_call_is_noise(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[170.0],3.0)
  r=row(ev,'exec_short');self.assertTrue(r['ok']);self.assertAlmostEqual(r['post_max_s'],1.9,1)
 def test_in_window_stall_counted(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[112.0],2.5)
  r=row(ev,'exec_short');self.assertAlmostEqual(r['excess_s'],1.4,1);self.assertFalse(r['ok'])
 def test_call_started_before_window_but_stalled_into_it_counts(self):
  ev=calls('exec_short',frange(10,90,1.3),1.1)+calls('exec_short',[99.0],4.0)  # began at 95, still running at 98
  self.assertFalse(row(ev,'exec_short')['ok'])
 def test_loop_cadence_is_not_a_pause(self):
  ev=calls('exec_stdin_large',frange(5,200,10.0),1.9)  # one 1 MiB exec every 10 s, each 1.9 s
  r=row(ev,'exec_stdin_large');self.assertLess(r['excess_s'],0.01);self.assertTrue(r['ok'])
 def test_loop_call_stall_still_counted(self):
  ev=calls('exec_stdin_large',frange(5,200,10.0),1.9)+calls('exec_stdin_large',[115.0],3.3)
  self.assertAlmostEqual(row(ev,'exec_stdin_large')['excess_s'],1.4,1)
 def test_send_stall_inside_a_transfer(self):
  ev=calls('send',frange(10,200,1.6),1.5)+calls('send',[118.0],4.5)  # one send held 3 s longer mid-update
  r=row(ev,'send');self.assertAlmostEqual(r['excess_s'],3.0,1);self.assertFalse(r['ok'])
 def test_send_noise_outside_window_ignored(self):
  ev=calls('send',frange(10,200,1.6),1.5)+calls('send',[160.0],4.0)
  self.assertTrue(row(ev,'send')['ok'])
 def test_stream_gap(self):
  steady=[t for t in frange(10,200,0.1) if not 112<t<114.1]  # 2.2 s gap inside the window
  r=row(ticks('exec_long',steady),'exec_long');self.assertAlmostEqual(r['excess_s'],2.0,delta=0.2);self.assertFalse(r['ok'])
  pre=[t for t in frange(10,200,0.1) if not 50<t<52.1]  # same gap, 50 s before the update
  r=row(ticks('exec_long',pre),'exec_long');self.assertLess(r['excess_s'],0.01);self.assertTrue(r['ok'])
 def test_failure_fails_the_verdict(self):
  ev=calls('preview',frange(10,200,0.3),0.12)+calls('preview',[115.0],5.0,ok=False)
  r=row(ev,'preview');self.assertEqual((r['failures'],r['failures_in_update']),(1,1));self.assertFalse(r['ok']);self.assertGreater(r['excess_s'],4)
 def test_no_data_in_window_fails(self):
  self.assertFalse(row(calls('exec_short',frange(10,90,1.3),1.1),'exec_short')['ok'])

 def test_noise_as_high_as_the_excess_is_unresolved_never_pass(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[60.0],3.0)+calls('exec_short',[112.0],2.5)  # noise 1.9, excess 1.4
  r=row(ev,'exec_short');self.assertEqual(r['status'],'UNRESOLVED');self.assertFalse(r['ok']);self.assertAlmostEqual(r['noise_s'],1.9,1)
 def test_excess_above_the_noise_still_fails(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[60.0],1.6)+calls('exec_short',[112.0],2.5)  # noise 0.5, excess 1.4
  self.assertEqual(row(ev,'exec_short')['status'],'FAIL')
 def test_excess_within_the_bar_passes_whatever_the_noise(self):
  ev=calls('exec_short',frange(10,200,1.3),1.1)+calls('exec_short',[60.0],3.0)+calls('exec_short',[112.0],1.9)  # excess 0.8
  self.assertEqual(row(ev,'exec_short')['status'],'PASS')
 def test_failure_is_never_unresolved(self):
  ev=calls('preview',frange(10,200,0.3),0.12)+calls('preview',[60.0],5.0)+calls('preview',[115.0],5.0,ok=False)
  self.assertEqual(row(ev,'preview')['status'],'FAIL')
 def test_pb_version_parse(self):
  self.assertEqual(excess.pb_version('noise\n{"data":{"version":"v1","commit":"abc"}}'),dict(version='v1',commit='abc'))
  self.assertIn('error',excess.pb_version('not json'))
if __name__=='__main__':unittest.main()
