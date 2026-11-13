# R-X-06: X-06 L<->W on dev-b71c48700, 2026-11-13 ~13:40-13:55 IST

Machines (own, not the shared fleet): laner-x06-l (Linux container on hp, user pbt) and laner-x06-w (hosted Windows runner winpool-6). Both `pb version`: dev-b71c48700 (installed with the build's own `pb install` over dev-918afefe9; the earlier run on dev-918afefe9 is below). Account: the hp operator account; both records revoked by exact id at the end.
Run: `xplat_config.py --only X-06 --devices L,W` (own repository host hp, ssh -R to the runner), result results/xplat-config-20261113T081648Z.json, 6/6 PASS:
- x06 L>W and W>L: two rules whose repository paths differ only by case refused (validate rc!=0, ok:false); two rules whose destinations differ only by case refused; 10 repository names (CON, prn, Aux.log, NUL, com1.txt, LPT9, a:b.txt, trail., "trail ", dir./f.txt) refused; the plain rule is accepted (control). On both laner-x06-l and laner-x06-w.
- x06_case_collision L>W and W>L: `Case-*.txt` and `case-*.txt` committed in a directory rule are reported as case_collision in `pb config sync status` skipped on BOTH machines and created on neither.
- x06_windows_case L>W and W>L: on W `C:\Users\X\...\App.json` and `c:\users\x\...\APP.json` as two rules are refused as one path.

Before the fix (dev-918afefe9, results/xplat-config-20261113T071424Z.json): x06 L>W PASS, x06_windows_case both PASS; x06_case_collision both directions FAIL (W never reported case_collision, 150-190 s): the collision was read from a disk walk and NTFS holds one of the two files. Fix b71c48700 reads it from the repository commit (TestTreeCaseCollisionsNamesBothSpellingsInTheMappedRule). x06 W>L failed once on the runner's ssh banner timeout (transport) and passed on the rerun.
