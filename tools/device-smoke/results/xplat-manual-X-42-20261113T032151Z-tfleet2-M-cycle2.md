# T-FLEET2: X-42 L<->M and M<->W on cycle 2 dev-e4d2cc0f8, 2026-11-13 08:45-08:51 IST

lanex2-l, lanex2-m (pbmac VM, re-paired 08:01 IST) and lanex2-w, all `pb version` dev-e4d2cc0f8, doctor healthy. Together with results/xplat-manual-X-42-20261113T005329Z-tfleet2-LW.md (L<->W on dev-94eb504aa: ping/wait, doctor, bundles, support references, completion bash/zsh/fish on L and PowerShell 5.1/7 on W, non-ASCII refusal) this covers every pair.
Raw output (commands run one at a time by the Python fleet harness; the temporary API token was held in memory and revoked by exact id apitok_a06f4ef6bb0a999d1da6cf777dae9f2b, rc 0):

## versions L dev-e4d2cc0f8 M dev-e4d2cc0f8 W dev-e4d2cc0f8
## part 1 ping and wait
lanex2-l>lanex2-m ping text rc 0 | summary: sent=4 received=4 loss=0.0% connect min/avg/max=37.97/41.39/48.41ms paths ['direct']
lanex2-l>lanex2-m ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['direct']
lanex2-l>lanex2-m wait runtime text rc 0 lanex2-m is ready for runtime (ready, generation 68). | json rc 0 ready runtime selected_path direct
lanex2-l>lanex2-m wait transport text rc 0 lanex2-m is ready for transport (ready, generation 68). | json rc 0 ready transport selected_path direct
lanex2-l>lanex2-m wait ssh text rc 0 lanex2-m is ready for ssh (ready, generation 68). | json rc 0 ready ssh selected_path direct
lanex2-m>lanex2-l ping text rc 0 | summary: sent=4 received=4 loss=0.0% connect min/avg/max=44.04/91.81/153.75ms paths ['regional_relay']
lanex2-m>lanex2-l ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['regional_relay']
lanex2-m>lanex2-l wait runtime text rc 0 lanex2-l is ready for runtime (ready, generation 64). | json rc 0 ready runtime selected_path relay
lanex2-m>lanex2-l wait transport text rc 0 lanex2-l is ready for transport (ready, generation 64). | json rc 0 ready transport selected_path relay
lanex2-m>lanex2-l wait ssh text rc 0 lanex2-l is ready for ssh (ready, generation 65). | json rc 0 ready ssh selected_path relay
lanex2-w>lanex2-m ping text rc 0 | summary: sent=4 received=4 loss=0.0% connect min/avg/max=251.61/315.82/503.25ms paths ['direct']
lanex2-w>lanex2-m ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['direct']
lanex2-w>lanex2-m wait runtime text rc 0 lanex2-m is ready for runtime (ready, generation 76). | json rc 0 ready runtime selected_path direct
lanex2-w>lanex2-m wait transport text rc 0 lanex2-m is ready for transport (ready, generation 76). | json rc 0 ready transport selected_path direct
lanex2-w>lanex2-m wait ssh text rc 0 lanex2-m is ready for ssh (ready, generation 76). | json rc 0 ready ssh selected_path direct
lanex2-m>lanex2-w ping text rc 0 | summary: sent=4 received=4 loss=0.0% connect min/avg/max=243.91/284.91/369.38ms paths ['direct']
lanex2-m>lanex2-w ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['direct']
lanex2-m>lanex2-w wait runtime text rc 0 lanex2-w is ready for runtime (ready, generation 65). | json rc 0 ready runtime selected_path direct
lanex2-m>lanex2-w wait transport text rc 0 lanex2-w is ready for transport (ready, generation 65). | json rc 0 ready transport selected_path direct
lanex2-m>lanex2-w wait ssh text rc 0 lanex2-w is ready for ssh (ready, generation 65). | json rc 0 ready ssh selected_path direct
ping json keys equal, same target lanex2-m from L and W sources: True
wait runtime json keys equal L-source vs W-source (target M): True ready ready
wait transport json keys equal L-source vs W-source (target M): True ready ready
wait ssh json keys equal L-source vs W-source (target M): True ready ready
## part 2 doctor on M
M doctor rc 0 Paperboat doctor: healthy ['peer_paths: pass: Paths: lanex2-w direct.']
M proxy-with-path rc 1 ['authentication: fail: The Paperboat server is unreachable through the configured proxy.', 'Recovery: Configure a credential-free http:// or https:// proxy URL with no path, query, or fragment, then retry `pb doctor`.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
M proxy-closed-port rc 1 ['authentication: fail: The Paperboat server did not accept the authenticated health check.', 'Recovery: Check network access and sign in again if needed, then run pb doctor again.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
## part 3 bugreports across M, L, W
bundles: M 488886 True support_a9e332ce-68f3-4f59-a683-4c6a9c868ccd | L 1184437 True support_eb49df55-f354-4a37-9377-24c70523f236 | W 362927 True support_476d2fee-2988-430b-bd89-b5d41530a84d
M>L scp rc 0
M>W scp rc 0
L>M scp rc 0
W>M scp rc 0
M bundle opened on L: testzip None files 4 utf8 ok crlf 0 non-ascii names []
contains '/home/tester/' False
contains '/Users/admin' False
contains 'runneradmin' False
contains 'lanex2-l' False
contains 'lanex2-w' False
secret-like (case-sensitive): []
M bundle opened on W: entries 4 utf8 ok; has /Users/admin False; has runneradmin False
L bundle opened on M: testzip None files 4 utf8 ok crlf 0 non-ascii names []
contains '/Users/admin' False
contains '/home/tester' False
contains 'runneradmin' False
contains 'lanex2-m' False
secret-like (case-sensitive): [] 
W bundle opened on M: testzip None files 4 utf8 ok crlf 0 non-ascii names []
contains '/Users/admin' False
contains '/home/tester' False
contains 'runneradmin' False
contains 'lanex2-m' False
secret-like (case-sensitive): [] 
M own bundle scan: testzip None files 4 utf8 ok crlf 0 non-ascii names []
contains '/Users/admin' False
contains 'Manageds-Virtual-Machine' False
contains 'lanex2-l' False
contains 'lanex2-w' False
secret-like (case-sensitive): []
## part 3b support refs resolve
M support_a9e332ce-68f3-4f59-a683-4c6a9c868ccd 200 recorded
L support_eb49df55-f354-4a37-9377-24c70523f236 200 recorded
W support_476d2fee-2988-430b-bd89-b5d41530a84d 200 recorded
token revoked apitok_a06f4ef6bb0a999d1da6cf777dae9f2b 0
## part 5 non-ASCII on M
M rename rc 2 pb: invalid machine name: machine name must be 1-63 ASCII letters, digits or hyphens, not starting or ending with a hyphen; e.g. zoe-build
Usage:
  pb
M add rc 2 pb: invalid machine name: machine name must be 1-63 ASCII letters, digits or hyphens, not starting or ending with a hyphen; e.g. zoe-build
Usage:
  pb
machines after: ['lanex2-l', 'lanex2-m', 'lanex2-w']

## Result
Passed: ping and `wait --for runtime|transport|ssh` text and --json in L>M, M>L, W>M, M>W (ready, same key sets across sources); doctor on M healthy, proxy wording identical to L and W (results/...-LW.md); a bundle made and uploaded on each OS opens on the other two (testzip ok, 4 files, UTF-8, no CRLF, no user paths (/Users/admin, /home/tester, C:/Users/runneradmin), no other machine names, no eyJ/private key/bearer match); all three support references resolve to `recorded`; non-ASCII name refused on M with the name rule, machine list unchanged.
Not producible on the fleet (same as the earlier M cell): a real clock-skew, relay or firewall doctor problem; their wording is pinned by unit tests (TestDoctorNamesTheClockOffset, TestDoctorSelectedPathNamesMixedPaths). Bash completion on the Mac needs brew bash 5 + bash-completion (named in `pb completion bash --help`); the Mac completion parts are the earlier M cell evidence.
