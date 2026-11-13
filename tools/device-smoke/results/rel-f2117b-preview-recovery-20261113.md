# F21-17 L: public preview through a control outage, recovery fixed (staging, 2026-11-13 IST, REL-F2117B)

Machine: pb dev-c5c155345 (container pb-rel-edge-a on anvit, staging account). Edges: paperboat-edge 2026.11.13.19 (edge-1 serves
public HTTPS on :18546). Server before: paperboat-server:stg-6ca6b6982. Server after: paperboat-server:stg-2d5e26b19
(2026.11.13.985826, built and started with deploy/staging.py build/up from 2d5e26b19: the same change on an older main; the pushed commit is the same diff on current main).

## Clean repro first (hostname-conflict hypothesis refuted)
Preview p1.rel-edge.stg.pprbt.dev (a child name under the verified domain: no apex preview, not the name the t1 tunnel uses),
one preview, same drill as the predecessor (edge-1 172.17.0.5 -> control :18443 dropped 90 s, GET every 2 s):
before 200 x3; during 90 s: 200=42 other=0; 25 s after control returned: 000 x5 (TLS internal error), still failing minutes later.
=> the failure does not need the apex/tunnel conflict.

## Root cause (live, in the staging database)
- No distribution action reached the edge after the cut; the edge still held the active certificate.
- preview_domains for p1: certificate_state ready -> renewing, certificate_failure_code edge_certificate_distribution_unavailable,
  written by recordPreviewCertificateFailure (server/internal/tunnelv1/certificate_worker.go) when the rebind/distribution
  attempt met the unreachable edge.
- ingressprojection/preview_ingress.go publishes a preview only while its domain is certificate_state='ready', so the next
  snapshot withdrew the live public decision; the edge then answers the name from its wildcard-only TLS config, which has no
  certificate for a custom domain.
- Decisive experiment: `UPDATE preview_domains SET certificate_state='ready'` for p1 (certificate unchanged): GET 200 within 4 s
  (before: 000).
Second gap, found while reading the edge: a revoke for a preview generation the edge never received (it missed the lease
renewals) matched no entry, so the older served generation stayed active and refused the next domain on the hostname with a
HostnameConflict (certificate_action_failed), also after a restart (the active certificate is persisted).
Red test first: TestRegistryRevokeOfANewerPreviewGenerationDropsTheServedOlderOne failed on CI run 43457311416.

## Fix
- server: a failed attempt keeps a ready, unexpired preview certificate ready (previewCertificateStateAfterFailure); the failure
  code is still recorded and the due query retries after its 1 minute cooldown.
- edge: Revoke of a preview lease also drops its older generations on the same edge process (supersededPreview).
Tests: TestPreviewDomainWithLiveCertificateStaysReadyWhenAnAttemptFails (server, PASS CI run 43460018534),
TestRegistryRevokeOfANewerPreviewGenerationDropsTheServedOlderOne (edge, PASS CI run 43460013890). 

## Drill on the fixed server (new preview p2.rel-edge.stg.pprbt.dev, same drill)
before 200 x3; drop 16:30:40Z, removed 16:32:11Z; during 90 s: 200=44 other=0 (edge control failures logged: 142);
after (25 s later, 5 x 3 s): 200 x5; 3 further GETs minutes later 200 x3; domain row certificate_state=ready (failure code
edge_certificate_distribution_unavailable recorded, preview still published).
Not live: the edge-side Revoke change (edges stay 2026.11.13.19 until the next edge rollout); unit-tested only.
