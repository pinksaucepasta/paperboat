# Status page email, proven live (2026-11-19)

Against the deployed Worker, with the provider's test recipient (a sink the provider documents for delivery tests; no inbox is read here, so "sent" means the provider accepted
the message, which is what the Worker records). Provider key: the same transactional-mail key the control plane uses, held as a Worker secret.

| Step | Request | Result |
|---|---|---|
| Subscribe | `POST /api/subscribe` with the test address | 200 `{ok:true}`; the row exists, unconfirmed, `last_mail` set (the confirmation mail went out) |
| Scanner prefetch | `GET /api/confirm?t=<token>` | a page with a `<form method="post">` only; the row stays unconfirmed |
| Confirm | `POST /api/confirm` (the form) | "You will get an email for incidents and maintenance."; row confirmed |
| Notify | admin `POST /api/admin/incidents` (status resolved, component website, `Idempotency-Key`) | 201, `mail: {sent: 1, failed: 0, truncated: false}` |
| Retried write | same key again | the same incident id, `replayed: true`; no second incident, no second mail |
| Feed | `GET /api/feed.xml` | contains `[resolved] Email check (test, ignore)` |
| Unsubscribe | `GET /api/unsubscribe?t=` then `POST /api/unsubscribe` | the GET changes nothing; the POST removes the row |
| Retract | the test incident and its update deleted from the database | incident count 0; the page's incident list and the feed no longer show it |

Left to check by eye: the provider dashboard lists the two messages (the confirmation, and the incident update).
