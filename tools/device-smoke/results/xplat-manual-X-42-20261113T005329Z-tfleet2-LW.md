# T-FLEET2: X-42 L<->W on main dev-94eb504aa, 2026-11-13 06:09-06:35 IST

Machines: lanex2-l (Linux container, bash 5.2/zsh 5.9/fish 3.7/bash-completion installed) and lanex2-w (fresh Windows runner winpool-3, Windows PowerShell 5.1 and pwsh 7.6.9).
`pb version` on both: dev-94eb504aa (main 94eb504aa). The Mac (lanex2-m) was offline (another lane's staging install at 06:04 IST), so every part below is the L<->W pair only; M parts are not in this file.
Raw run output below is trimmed: commands run by the Python fleet harness one at a time (`pb` calls via ssh), nothing else changed on the machines.

## versions
L dev-94eb504aa W dev-94eb504aa
## part 1 ping and wait
lanex2-l>lanex2-w ping text rc 0 | sample 1: path=direct connect=411.37ms | sample 2: path=direct connect=202.98ms | sample 3: path=direct connect=203.59ms | sample 4: path=direct connect=202.58ms | summary: sent=4 received=4 loss=0.0% connect min/avg/max=202.58/255.13/411.37ms
lanex2-l>lanex2-w ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['direct']
lanex2-l>lanex2-w wait runtime text rc 0 lanex2-w is ready for runtime (ready, generation 53).
lanex2-l>lanex2-w wait runtime json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'runtime'} selected_path direct
lanex2-l>lanex2-w wait transport text rc 0 lanex2-w is ready for transport (ready, generation 53).
lanex2-l>lanex2-w wait transport json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'transport'} selected_path direct
lanex2-l>lanex2-w wait ssh text rc 0 lanex2-w is ready for ssh (ready, generation 53).
lanex2-l>lanex2-w wait ssh json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'ssh'} selected_path direct
lanex2-w>lanex2-l ping text rc 0 | sample 1: path=direct connect=402.55ms | sample 2: path=direct connect=203.86ms | sample 3: path=direct connect=203.00ms | sample 4: path=direct connect=201.09ms | summary: sent=4 received=4 loss=0.0% connect min/avg/max=201.09/252.62/402.55ms
lanex2-w>lanex2-l ping json rc 0 {'schema': 'paperboat.ping/v1', 'sent': 4, 'received': 4, 'lost': 0} paths ['direct']
lanex2-w>lanex2-l wait runtime text rc 0 lanex2-l is ready for runtime (ready, generation 55).
lanex2-w>lanex2-l wait runtime json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'runtime'} selected_path direct
lanex2-w>lanex2-l wait transport text rc 0 lanex2-l is ready for transport (ready, generation 55).
lanex2-w>lanex2-l wait transport json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'transport'} selected_path direct
lanex2-w>lanex2-l wait ssh text rc 0 lanex2-l is ready for ssh (ready, generation 55).
lanex2-w>lanex2-l wait ssh json rc 0 {'schema': 'paperboat.wait-result/v1', 'outcome': 'ready', 'condition': 'ssh'} selected_path direct
ping json key sets L-source vs W-source equal: True
wait runtime json key sets L-source vs W-source equal: True outcomes ready ready
wait transport json key sets L-source vs W-source equal: True outcomes ready ready
wait ssh json key sets L-source vs W-source equal: True outcomes ready ready
## part 2 doctor
lanex2-l doctor rc 0 | Paperboat doctor: healthy | peer line: ['peer_paths: pass: Paths: lanex2-w direct.']
lanex2-l proxy-with-path rc 1 | ['authentication: fail: The Paperboat server is unreachable through the configured proxy.', 'Recovery: Configure a credential-free http:// or https:// proxy URL with no path, query, or fragment, then retry `pb doctor`.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
lanex2-l proxy-closed-port rc 1 | ['authentication: fail: The Paperboat server did not accept the authenticated health check.', 'Recovery: Check network access and sign in again if needed, then run pb doctor again.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
lanex2-w doctor rc 0 | Paperboat doctor: healthy | peer line: ['peer_paths: pass: Paths: lanex2-l direct.']
lanex2-w proxy-with-path rc 1 | ['authentication: fail: The Paperboat server is unreachable through the configured proxy.', 'Recovery: Configure a credential-free http:// or https:// proxy URL with no path, query, or fragment, then retry `pb doctor`.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
lanex2-w proxy-closed-port rc 1 | ['authentication: fail: The Paperboat server did not accept the authenticated health check.', 'Recovery: Check network access and sign in again if needed, then run pb doctor again.', 'clock: warning: The Paperboat server could not be reached to read its clock.', 'Recovery: Check the connection to Paperboat and run `pb doctor` again.']
## part 3 bugreport
L bundle 1186088 uploaded True support_26ddf7ca-687c-40d0-90cb-4e4a5264183d
W bundle 74952 uploaded True support_d99155b0-4fbb-4606-8518-0e5ec91cb1f1
W>L scp bundle rc 0
L>W scp bundle rc 0
W bundle opened on L:
testzip None
files ['manifest.json', 'recent-events.ndjson', 'events.ndjson', 'status.json']
utf8 ok; CRLF count 0 non-ascii names []
contains '/home/tester' False
contains 'tester' False
contains 'runneradmin' False
contains 'lanex2-m' False
secret-like: []
L bundle scanned on L:
testzip None
files ['manifest.json', 'recent-events.ndjson', 'events.ndjson', 'status.json']
utf8 ok; CRLF count 0 non-ascii names []
contains '/home/tester' False
contains 'tester' False
contains 'runneradmin' False
contains 'lanex2-m' False
secret-like: [b'EYJBY2FGKACTSBEZPV66GLBTPG', b'EYJBY2FGKACTSBEZPV66GLBTPG']
L bundle opened on W:
entry manifest.json 375
entry recent-events.ndjson 269414
entry events.ndjson 20826136
entry status.json 6490
entries 4 utf8 ok; contains tester False; runneradmin False #< CLIXML
<Objs Version="1.1.0.1" xmlns="http://schemas.microsoft.com/powershell/2004/04"><Obj S="progress" RefId="0"><TN RefId="0"><T>System.Management.Automation.PSCustomObject</T><T>System.Object<
W bundle own size 74952
W bundle path scan (copy on L):
testzip None
files ['manifest.json', 'recent-events.ndjson', 'events.ndjson', 'status.json']
utf8 ok; CRLF count 0 non-ascii names []
contains 'runneradmin' False
contains 'C:\\\\Users' False
secret-like: []
{"L": "support_26ddf7ca-687c-40d0-90cb-4e4a5264183d", "W": "support_d99155b0-4fbb-4606-8518-0e5ec91cb1f1"}
## part 5 non-ASCII
L rename rc 2 pb: invalid machine name: machine name must be 1-63 ASCII letters, digits or hyphens, not starting or ending with a hyphen; e.g. zoe-build
Usage:
  pb machine rename <machine> <name> [flags]

Flags:
 
L add rc 2 pb: invalid machine name: machine name must be 1-63 ASCII letters, digits or hyphens, not starting or ending with a hyphen; e.g. zoe-build
Usage:
  pb machine add [flags]

Flags:
  -h, --help         
W rename rc=2
W add rc=2
machines after: ['lanex2-l', 'lanex2-m', 'lanex2-w', 'rel-x-p', 'xd27-l', 'xd27-m', 'xd27-m-2', 'xd27-m-3', 'xd27-m-4', 'xd27-m-5', 'xd27-w']

## Part 3b support reference resolves (API token created on L, in memory only, revoked by exact id apitok_8139894484362cc62f0e7ef159069339, rc 0)
GET /v1/support/<reference> with the token: L bundle support_26ddf7ca-687c-40d0-90cb-4e4a5264183d -> 200 status recorded, reporting diagnostic_bundle; W bundle support_d99155b0-4fbb-4606-8518-0e5ec91cb1f1 -> 200 status recorded, reporting diagnostic_bundle.
Secret scan note: a case-insensitive regex flagged 'EYJBY2FGKACTSBEZPV66GLBTPG' in the L bundle; context is `"step_id":"EYJ..."` (a 26-char base32 step id, not a JWT). Case-sensitive scan for eyJ tokens, PRIVATE KEY and bearer tokens: no hit.

## Part 3c long paths, Unicode
W bundle copied by `pb scp` from W to L into `/home/tester/tf2-d\u00e9ep/<60 a>/<60 b>/<60 c>/bundle-w.zip` (217 chars): opens with python zipfile, testzip None, 4 files, all UTF-8.
L bundle copied from L to W into `C:/Users/runneradmin/tf2-d\u00e9ep/<50>/<50>/<50>/bundle-l.zip` (195 chars): opens with System.IO.Compression, 4 entries, all decoded as UTF-8. Scratch folders removed. Neither bundle holds CRLF (count 0) or Unicode names.

## Part 4 completion (hyphenated names, prefix `lanex2-`)
- L bash 5.2 with bash-completion: `__start_pb` COMPREPLY: lanex2-l lanex2-m lanex2-w
- L zsh 5.9 (`_pb` with _describe/compadd stubs): lanex2-l:lanex2-l - online, lanex2-m (offline), lanex2-w - online
- L fish 3.7: lanex2-l, lanex2-m, lanex2-w with descriptions
- W Windows PowerShell 5.1.26100.33648 (`CompleteInput('pb exec lanex2-')`): lanex2-l lanex2-m lanex2-w; W pwsh 7.6.9: same.
