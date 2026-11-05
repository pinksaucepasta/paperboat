#!/usr/bin/env python3
# python3 excess_test.py: synthetic events through excess.score (update at t=100..130, window 98..140).
import sys,pathlib,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parent));import excess
U=[(100.0,130.0)]
def calls(f,ends,ms,ok=True):return [dict(f=f,t=t,ok=ok,ms=int(ms*1000)) for t in ends]
def ticks(f,times):return [dict(f=f,t=t,ok=True,ms=None) for t in times]
def small_sends():return calls('send',frange(10,200,1.0),0.4)  # the 64 KiB series: one send a second, 0.4 s each
def row(ev,f):return {r['feature']:r for r in excess.score(ev,U)}[f]
frange=lambda a,b,s:[a+i*s for i in range(int((b-a)/s)+1)]
def fetches(a,b,skip=None,f='preview'):  # a fetch every 0.3 s taking 0.1 s; none completes inside skip=(from,to)
 return calls(f,[t for t in frange(a,b,0.3) if not skip or not skip[0]<t<skip[1]],0.1)

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
  ev=small_sends()+calls('send',[118.0],3.4)  # one send held 3 s longer mid-update
  r=row(ev,'send');self.assertAlmostEqual(r['excess_s'],3.0,1);self.assertFalse(r['ok'])
 def test_send_noise_outside_window_ignored(self):
  ev=small_sends()+calls('send',[160.0],3.0)
  self.assertTrue(row(ev,'send')['ok'])
 def test_stream_gap(self):
  steady=[t for t in frange(10,200,0.1) if not 112<t<114.1]  # 2.2 s gap inside the window
  r=row(ticks('exec_long',steady),'exec_long');self.assertAlmostEqual(r['excess_s'],2.0,delta=0.2);self.assertFalse(r['ok'])
  pre=[t for t in frange(10,200,0.1) if not 50<t<52.1]  # same gap, 50 s before the update
  r=row(ticks('exec_long',pre),'exec_long');self.assertLess(r['excess_s'],0.01);self.assertTrue(r['ok'])
 def test_failure_fails_the_verdict(self):
  ev=fetches(10,200,skip=(110,115))+calls('preview',[115.0],5.0,ok=False)  # one fetch timed out after 5 s
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
 def test_send_overlapping_the_window_counts(self):
  ev=small_sends()+calls('send',[99.0],2.4)  # began at 96.6, still running at the window's start (98)
  r=row(ev,'send');self.assertAlmostEqual(r['excess_s'],2.0,1);self.assertFalse(r['ok'])
 def test_slow_send_after_the_window_does_not_count(self):
  ev=small_sends()+calls('send',[160.0],3.0)  # window ends at 140
  r=row(ev,'send');self.assertTrue(r['ok']);self.assertAlmostEqual(r['post_max_s'],2.6,1);self.assertLess(r['excess_s'],0.3)
 def test_a_1_2_second_stall_inside_a_send_fails(self):
  ev=small_sends()+calls('send',[115.0],1.6)  # a 0.4 s send held 1.2 s mid-transfer
  r=row(ev,'send');self.assertAlmostEqual(r['excess_s'],1.2,1);self.assertEqual(r['status'],'FAIL')
 def test_a_send_stall_under_the_bar_passes(self):
  ev=small_sends()+calls('send',[115.0],1.3)  # held 0.9 s
  self.assertEqual(row(ev,'send')['status'],'PASS')
 def test_fetch_loop_scores_the_outage_between_successes(self):
  r=row(fetches(10,200,skip=(111.0,112.5)),'preview')  # no fetch succeeds for 1.5 s inside the window
  self.assertAlmostEqual(r['excess_s'],1.5,delta=0.2);self.assertEqual(r['status'],'FAIL')
  r=row(fetches(10,200,skip=(111.0,111.9),f='tunnel'),'tunnel')  # a 0.9 s outage is inside the bar
  self.assertEqual(r['status'],'PASS')
 def test_fetch_outage_outside_the_window_is_noise(self):
  r=row(fetches(10,200,skip=(40.0,43.0)),'preview');self.assertTrue(r['ok']);self.assertGreater(r['pre_max_s'],2)
  r=row(fetches(10,200,skip=(170.0,173.0)),'preview');self.assertTrue(r['ok']);self.assertGreater(r['post_max_s'],2)
 def test_fetch_loop_latency_alone_is_not_an_outage(self):
  self.assertTrue(row(calls('preview',frange(10,200,0.3),0.25),'preview')['ok'])  # slow but steady fetches
 def test_send_big_is_a_throughput_check_not_a_stall_measure(self):
  big=lambda ends,ms,ok=True:[dict(f='send_big',t=t,ok=ok,ms=int(ms*1000),bytes=20000000) for t in ends]
  r=row(big(frange(10,200,10.0),4.0)+big([115.0],9.0),'send_big')  # a slow 20 MB send is not a verdict by itself
  self.assertEqual(r['status'],'PASS');self.assertIn('5.0 MB/s before, lowest in window 2.2',r['info'])
  self.assertEqual(row(big(frange(10,200,10.0),4.0)+big([115.0],60.0,ok=False),'send_big')['status'],'FAIL')
 def test_failed_calls_report_codes_only(self):
  ev=small_sends()+[dict(f='send',t=115.0,ok=False,ms=300,rc=1,err='transfer_failed:upload:network')]*2+[dict(f='send',t=120.0,ok=False,ms=300,rc=2)]
  r=row(ev,'send');self.assertEqual(r['errors'],{'transfer_failed:upload:network':2,'exit2':1});self.assertEqual(r['status'],'FAIL')
 def test_bugreport_id_parse(self):
  self.assertEqual(excess.bugreport_id('x\n{"schema":"s","correlation_id":"c-1"}'),dict(correlation_id='c-1'))
  self.assertEqual(excess.bugreport_id('{"error":{"code":"daemon_down","support_reference":"S-9"}}'),dict(error='daemon_down',support_reference='S-9'))
  self.assertIn('error',excess.bugreport_id('not json'))
if __name__=='__main__':unittest.main()
