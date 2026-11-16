# FIX-FIRSTSYNC device check, 2026-11-16 (IST evening)

Fix 55c2d21fe. lanex2-l2 (writer) dev-70dd9ddd4 (contains the fix), lanex2-w (receiver) dev-c688afd25; server .39; under fleet-configsync.lock.

```
L dev-70dd9ddd4
W dev-c688afd25
PASS 400 of 400 files written during the first sync were published and reached lanex2-w (21 sampled)
rc=0
```

First attempt on the same builds published 400 of 400 but lanex2-w lagged past a 120 s wait; second run with a 300 s wait passed.
