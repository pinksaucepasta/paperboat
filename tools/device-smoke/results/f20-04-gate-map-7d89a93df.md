# F20-04 Windows gate on current main (build to result files)

- origin/main commit: 7d89a93df23de1034379e337313d4587a33ba8a2 (ancestors checked with `git merge-base --is-ancestor`: ff91c1218, 0de0bd1ce, 8d4886763: all yes)
- installed on the runner with the build's own `pb install` over the previous build, pairing kept: version `dev-7d89a93df`, binary sha256 `fb6c8d60c9c0debe8dac1ad092767aadc55819e91fb907c4f202937fd49517c1`
- machine: winpool-11 (GitHub-hosted Windows Server 2025), paired as winpool-k-b160f4, peer hp, IIS stopped; `windows_checks.py --install-json <receipt> --machine winpool-k-b160f4 --peer hp --only <id>`, one check at a time, all first-try PASS, no reruns
- audit of the same commit: f20-04-audit-7d89a93df.txt (command and output)

| check | result | seconds | result file |
|---|---|---|---|
| tunnel-manage | PASS | 115 | windows-20261110T215120Z.json |
| tunnel-lifecycle | PASS | 171 | windows-20261110T215426Z.json |
| tunnel-survival | PASS | 343 | windows-20261110T220024Z.json |
| tunnel-protocols | PASS | 58 | windows-20261110T220351Z.json |
| tunnel-transfer | PASS | 55 | windows-20261110T220907Z.json |
| tunnel-public-protocols | PASS | 127 | windows-20261110T221542Z.json |
| tunnel-failures | PASS | 283 | windows-20261110T222332Z.json |
| preview-survival | PASS | 159 | windows-20261110T222644Z.json |
| local-access (6/6) | PASS | 137 | windows-20261110T223127Z.json |

Earlier builds' files (dev-c4ce5d7ed gate, the a58de7919 / e2c21b131 tunnel-lifecycle comparison) are listed in the CHECKLIST F20-04 evidence.
