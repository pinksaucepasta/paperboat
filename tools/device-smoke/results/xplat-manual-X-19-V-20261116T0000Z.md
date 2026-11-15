# X-19 re-verification on one build of main (lane V-X-19), 2026-11-15/16

Build: `dev-ccb2522a2` (origin/main ccb2522a2, contains 900bff252 and the macOS guard change), built by `~/bin/pb-build.sh`, installed with each machine's own `pb install`.
`pb version` on every machine of the run: rx19d-a (Linux amd64 container, account A) `dev-ccb2522a2`; rx19d-b (Linux amd64 container, account B) `dev-ccb2522a2`; rx19d-m (macpool-2, macOS arm64, account A) `dev-ccb2522a2`; rx19d-w (winpool-2, Windows amd64, account B) `dev-ccb2522a2`.
Own throwaway machines only (two fresh accounts, containers on pbvm2, macpool-2, winpool-2); no shared fleet or apple.

| Part | macOS | Windows | Linux pairs |
| --- | --- | --- | --- |
| `pb doctor` rebind cause and workaround with a real rebind-filtering resolver | PASS | PASS | n/a |
| Browser with Secure DNS, both URL forms, OS trust store | PASS (Chrome, Safari) | PASS (Edge) | n/a |
| Team grant revoke and team setting off end held private TCP | 6 of 6 pairs, both mutations PASS | | |

## Doctor with a rebind filter (macOS and Windows)
A stdlib UDP resolver on 127.0.0.1:53 forwards to 1.1.1.1 and answers NXDOMAIN for private/loopback A records (it logs `REBINDFILTER NXDOMAIN private A`). The adapter DNS is pointed at it (macOS `networksetup -setdnsservers Ethernet 127.0.0.1`; Windows `Set-DnsClientServerAddress` on the default-route adapter, ifIndex 14), a detached timer restores it after 330 s, and it is restored by hand at the end (macOS: no servers set before and after; Windows: 168.63.129.16 before and after). Target: rx19d-a / rx19d-b with a persistent private policy to `http://127.0.0.1:24191`.

Both OSes, `pb doctor --json` `local_names`: baseline `pass`; filtered `warning`: "Your DNS resolver blocks private answers (DNS rebinding protection), so names under <domain> do not resolve here." with recovery "Allow <domain> in your router or Pi-hole, or use the machine's 127.100.x.y address (`pb resolve <machine>`) with `pb access machine`. Machine access with pb does not use these names."; restored `pass`. While filtered: `curl` cannot resolve `24191.<machine>.<domain>`, the assigned address `http://127.100.0.2:24191/` and `pb access machine <m> --port 24191 --listen 127.0.0.1:24192` both return the marker byte-exact.

### macOS (results of the run)
```text
### version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-ccb2522a2
DNS before: There aren't any DNS Servers set on Ethernet.
### baseline
[{'category': 'local', 'code': 'local_names', 'status': 'pass', 'summary': 'Local browser names resolve to this computer.'}]
### filtered
[{'category': 'local', 'code': 'local_names', 'status': 'warning', 'summary': 'Your DNS resolver blocks private answers (DNS rebinding protection), so names under ppr.boats do not resolve here.', 'recovery': "Allow ppr.boats in your router or Pi-hole, or use the machine's 127.100.x.y address (`pb resolve <machine>`) with `pb access machine`. Machine access with pb does not use these names."}]
### curl filtered
curl: (6) Could not resolve host: 24191.rx19d-a.ppr.boats
### assigned IP
b'rx19d-browser-byte-exact\n'
### explicit access
http://127.0.0.1:24192/ b'rx19d-browser-byte-exact\n'
### restore
bash: line 19: 11672 Terminated: 15          nohup sh -c 'sleep 330; networksetup -setdnsservers Ethernet Empty; dscacheutil -flushcache; killall -HUP mDNSResponder; sudo pkill -f rebind.py' > /dev/null 2>&1
bash: line 19: 11840 Terminated: 15          sudo -n nohup python3 ~/rebind.py > ~/rebind.log 2>&1
DNS after: There aren't any DNS Servers set on Ethernet.
[{'category': 'local', 'code': 'local_names', 'status': 'pass', 'summary': 'Local browser names resolve to this computer.'}]
### rebind log
   2 REBINDFILTER NXDOMAIN private A
```

### Windows
```text
### version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-ccb2522a2
ifIndex 14 before: 168.63.129.16
restore timer pid 8476
### baseline
[{'category': 'local', 'code': 'local_names', 'status': 'pass', 'summary': 'Local browser names resolve to this computer.'}]
### filtered
[{'category': 'local', 'code': 'local_names', 'status': 'warning', 'summary': 'Your DNS resolver blocks private answers (DNS rebinding protection), so names under ppr.boats do not resolve here.', 'recovery': "Allow ppr.boats in your router or Pi-hole, or use the machine's 127.100.x.y address (`pb resolve <machine>`) with `pb access machine`. Machine access with pb does not use these names."}]
### curl filtered
### assigned IPcurl.exe : curl: (6) Could not resolve host: 24191.rx19d-b.ppr.boats
 

b'rx19d-browser-byte-exact\n'
### explicit access
http://127.0.0.1:24192/ b'rx19d-browser-byte-exact\n'
### restore
ifIndex 14 after: 168.63.129.16
[{'category': 'local', 'code': 'local_names', 'status': 'pass', 'summary': 'Local browser names resolve to this computer.'}]
```

## Browsers with Secure DNS
Chromium profiles (Chrome on macOS, Edge on Windows) are fresh and their `Local State` carries `{"dns_over_https":{"mode":"secure","templates":"https://cloudflare-dns.com/dns-query"}}`; no certificate bypass flag; the CA is the one `pb install` put in the OS store (`curl` without `-k` returns the marker on both OSes). Both URL forms load: `https://24191.<machine>.<domain>/` and `https://<machine>.<domain>:24191/`. The net log shows `secure_dns_mode` 2, the DoH template, and the name answered `127.100.0.1`. The target origin log shows each request.
On the Windows runner the Windows HTTP service (HTTP.sys, PID 4) owns port 80/443, so `pb doctor` says browser URLs are off and names it; after `net stop http /y` the gateway binds 127.100.0.1:443 by itself within about a minute (pairs and browsers pass after that, as recorded for dev-900bff252).
```text
{"machine_id":"machine_13a740e4-dd87-48ae-9b09-fabb8bee847a","alias":"rx19d-a","assigned_ip":"127.100.0.2","forwarded_ports":[24191,38693],"browser_urls":{"24191":"https://24191.rx19d-a.ppr.boats","38693":"https://38693.rx19d-a.ppr.boats"}}
curl no -k: rx19d-browser-byte-exact
== net0
DNS_CONFIG_CHANGED {'secure_dns_mode': 2, 'doh_config': {'servers': [{'template': 'https://cloudflare-dns.com/dns-query'}]}, 'nameservers': ['192.168.64.1:53']}
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('24191.rx19d-a.ppr.boats', None)]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('24191.rx19d-a.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
== net1
DNS_CONFIG_CHANGED {'secure_dns_mode': 2, 'doh_config': {'servers': [{'template': 'https://cloudflare-dns.com/dns-query'}]}, 'nameservers': ['192.168.64.1:53']}
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('_24191._https.rx19d-a.ppr.boats', None)]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('rx19d-a.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
--- Windows (Edge headless, --dump-dom)
### version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-ccb2522a2
More help is available by typing NET HELPMSG 3521.
{"machine_id":"machine_fe5ce8fe-551d-4662-bd76-11d1310cd55c","alias":"rx19d-b","assigned_ip":"127.100.0.2","forwarded_ports":[24191,38693],"browser_urls":{"24191":"https://24191.rx19d-b.ppr.boats","38693":"https://38693.rx19d-b.ppr.boats"}}
curl.exe no -k: rx19d-browser-byte-exact
### edge 0 https://24191.rx19d-b.ppr.boats/?v19=edge-prefix
<html><head></head><body>rx19d-browser-byte-exact
</body></html>

DNS_CONFIG_CHANGED {'secure_dns_mode': 0, 'doh_config': {'servers': []}, 'nameservers': ['168.63.129.16:53']}
DNS_CONFIG_CHANGED {'secure_dns_mode': 2, 'doh_config': {'servers': [{'template': 'https://cloudflare-dns.com/dns-query'}]}, 'nameservers': ['168.63.129.16:53']}
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('24191.rx19d-b.ppr.boats', None)]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('24191.rx19d-b.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('24191.rx19d-b.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
### edge 1 https://rx19d-b.ppr.boats:24191/?v19=edge-explicit
<html><head></head><body>rx19d-browser-byte-exact
</body></html>

DNS_CONFIG_CHANGED {'secure_dns_mode': 0, 'doh_config': {'servers': []}, 'nameservers': ['168.63.129.16:53']}
DNS_CONFIG_CHANGED {'secure_dns_mode': 2, 'doh_config': {'servers': [{'template': 'https://cloudflare-dns.com/dns-query'}]}, 'nameservers': ['168.63.129.16:53']}
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('rx19d-b.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('rx19d-b.ppr.boats', [{'address': '127.100.0.1', 'port': 0}])]
HOST_RESOLVER_DNS_TASK_EXTRACTION_RESULTS [('_24191._https.rx19d-b.ppr.boats', None)]
--- origin log of rx19d-a (Chrome, Safari from macOS) and rx19d-b (Edge from Windows)
127.0.0.1 - - [15/Nov/2026 22:18:43] "GET /?v19=chrome-prefix HTTP/1.1" 200 -
127.0.0.1 - - [15/Nov/2026 22:19:08] "GET /?v19=chrome-explicit HTTP/1.1" 200 -
127.0.0.1 - - [15/Nov/2026 22:19:46] "GET /?v19=safari-prefix HTTP/1.1" 200 -
127.0.0.1 - - [15/Nov/2026 22:19:50] "GET /?v19=safari-explicit HTTP/1.1" 200 -
127.0.0.1 - - [15/Nov/2026 22:20:23] "GET /?v19=edge-prefix HTTP/1.1" 200 -
127.0.0.1 - - [15/Nov/2026 22:20:28] "GET /?v19=edge-explicit HTTP/1.1" 200 -
```

## Team revoke ends held flows (xplat_team_private_end.py, result xplat-manual-X-19-team-20261115T222137Z.json)
Holder OS > shared target OS; throwaway paid team, two accounts. Each mutation holds a `pb access machine` forward and a raw `127.100.x.y` TCP flow, both echoing first; both must end within 30 s and a fresh attempt must be refused.

| Pair | Grant revoke (s) | Setting off (s) | Result |
|---|---:|---:|---|
| L>M | 11.22 | 5.93 | PASS |
| M>L | 12.87 | 7.63 | PASS |
| L>W | 9.52 | 5.51 | PASS |
| W>L | 15.71 | 8.79 | PASS |
| M>W | 13.33 | 7.60 | PASS |
| W>M | 19.21 | 8.07 | PASS |

The only code change of this lane is in the script: the build id it requires is now `XPE_BUILD` instead of a hard-coded older build.
