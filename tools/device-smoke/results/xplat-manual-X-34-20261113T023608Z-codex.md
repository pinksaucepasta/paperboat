# X-34 non-owner policy parity attempt — 2026-11-13 UTC

Result: NOT PROVEN. Existing team member is inactive; authorization rejects the workspace before evaluating a rule. CHECKLIST unchanged.

## Machines and build
- lanex2-l (Linux target/owner): `pb version` -> Version dev-e4d2cc0f8 (rc 0).
- lanex2-w (Windows target/owner): `pb version` -> Version dev-e4d2cc0f8 (rc 0).
- xa2-l2 (Linux non-owner client): `pb version` -> Version dev-d24378a95 (rc 0). Existing fixture build differs; no installs performed.

Fleet access used xplat_fleet.linux()/windows(), with fleet.env sourced. Member uses ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2; pb path /home/tester/.local/bin/pb.

## Existing harness run
`python3 policy_team.py --team paperboat-smoke-365ac02bcda9 --machine-id machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a --machine-name lanex2-l --owner-cmd 'ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l' --member-cmd 'ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2' --owner-pb /home/tester/.local/bin/pb --member-pb /home/tester/.local/bin/pb`

Executed existing main() unchanged, with sh() wrapped only to record command/output. Exit 1. Team get before share returned generation 88, member role=member active=false, owner active=true; saved policy version 25. In-file failing test produced a validation refusal, but live allow never worked. Apparent denial passes cannot establish policy enforcement.

### Harness results (emails redacted)
```
PASS owner reads the team policy
PASS share the owner machine into the team
PASS no rule: the member is refused
PASS simple view: grant exec to every member
FAIL the member can exec: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_cc85bdab-e9de-4c4d-b28d-ec81b0d53c33.
PASS the team policy is plain JSON to extend
FAIL F18-05 the review lists the member losing exec, nothing saved: Tests: 0 passed, 0 failed
PASS F18-04 a failing in-file test blocks the save
FAIL F18-04 who-can lists the member while the grant allows it: ['user:<redacted-email>']
PASS a deny rule is saved after the access review
PASS deny wins over the grant: the member is refused
FAIL explain names the deny rule: {}
PASS F18-04 who-can no longer lists the member
PASS F18-05 a renamed machine reads with its new name in the policy
PASS F18-04 the owner guard holds against any deny (personal draft)
PASS F18-04 what-can has no exec on the machine for the member
FAIL F18-04 explain on a draft without the deny allows the member: {}
FAIL F18-08 a member reads the policy and cannot edit everything: rt reference: support_b9a0fd9c-14be-401b-8489-a7c3b6a35785.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_b9a0fd9c-14be-401b-8489-a7c3b6a35785"}}
FAIL F18-08 a member cannot add a rule on another member's machine: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_a4aacdf3-3b4d-4547-ba93-391835c1295a.
FAIL F18-08 a member explains their own access: rt reference: support_95d893ce-0baa-472c-9f78-6793720d8011.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_95d893ce-0baa-472c-9f78-6793720d8011"}}
FAIL F18-08 a member cannot explain someone else's access: ge":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_71a2fab2-645a-4afe-a992-dd7aeb506e55.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_71a2fab2-645a-4afe-a992-dd7aeb506e55"}}
FAIL F18-07 pb team grant gives the member the team ENV (team ENV must exist): ge":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_4e2223e1-774e-430a-9710-3f5643f99193.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_4e2223e1-774e-430a-9710-3f5643f99193"}}
PASS restore the starting policy version
PASS F18-05 saves and restores are audit events
PASS unshare the machine
RESULT FAIL the member can exec,F18-05 the review lists the member losing exec, nothing saved,F18-04 who-can lists the member while the grant allows it,explain names the deny rule,F18-04 explain on a draft without the deny allows the member,F18-08 a member reads the policy and cannot edit everything,F18-08 a member cannot add a rule on another member's machine,F18-08 a member explains their own access,F18-08 a member cannot explain someone else's access,F18-07 pb team grant gives the member the team ENV (team ENV must exist)
```

### Relevant exact commands and trimmed output
```
COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_36ebf6cc-b299-48ee-abb3-44eda65deb57.

PASS no rule: the member is refused

COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_e72da72e-5622-4739-812d-ff6ee87e8485.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_f56bbd51-1cc7-4ed8-b7b9-7078202590e7.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_d38d8ae8-8f18-4673-a178-332ccd0e09e0.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_074087e6-9dfe-46f1-aa25-d0ea869fe122.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_cdc4dc14-3606-4e7f-b344-34f8776bee6e.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_0bf7e067-7295-4e62-84e0-f6f42b7108b2.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_9e3fae24-0bb4-4a05-9d17-965462404185.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_6bd02d1a-1658-41bf-9127-c888ad60549f.


COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_cc85bdab-e9de-4c4d-b28d-ec81b0d53c33.

FAIL the member can exec: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_cc85bdab-e9de-4c4d-b28d-ec81b0d53c33.

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy set - --workspace paperboat-smoke-365ac02bcda9 --dry-run rc 0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)

FAIL F18-05 the review lists the member losing exec, nothing saved: Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes rc 2
/tests/0/src: unknown_subject: "user:<redacted-email>" is not a member of this workspace (a test names one user:<email>)
Tests: 0 passed, 1 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
pb: the policy is not valid: 1 error(s)
Usage:
  pb policy set <file|-> [flags]

Examples:
  pb policy set policy.hujson
  pb policy set policy.hujson --dry-run
  pb policy get | pb policy set - --yes --message "tidy"

Flags:
      --dry-run          validate and print the changes without saving
  -h, --help             help for set
      --message string   describe the change in the version history
      --yes              confirm the printed access changes without asking

Global Flags:
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug

PASS F18-04 a failing in-file test blocks the save

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-l --action exec --json rc 0
{
  "who_can": [
    {
      "subject": "user:<redacted-email>",
      "target": "machine:lanex2-l",
      "action": "exec",
      "reason": "rule",
      "rules": [
        "/rules/4"
      ]
    }
  ]
}

FAIL F18-04 who-can lists the member while the grant allows it: ['user:<redacted-email>']

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy set - --workspace paperboat-smoke-365ac02bcda9 --message f18-team-check --yes rc 0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
Saved policy version 27 (sha256:c1d9f3781c231f2d234e4023543c1b0a73259062560235ebdb5fa8d734b6f806).

PASS a deny rule is saved after the access review

COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_c91807fe-b8ec-49e8-ae15-0807e87e1a6a.

PASS deny wins over the grant: the member is refused

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json rc 1
{"schema_version":"1.0","ok":false,"error":{"code":"invalid_policy","category":"usage","message":"The policy file is not valid. Fix the listed errors and try again. Support reference: support_5b1b85e6-5c89-40cf-960b-b416fee5c185.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_5b1b85e6-5c89-40cf-960b-b416fee5c185"}}

FAIL explain names the deny rule: {}

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-l --action exec --json rc 0
{}

PASS F18-04 who-can no longer lists the member

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace personal --src user:<redacted-email> --dst machine:lanex2-l --action exec --file /tmp/f18-deny-all.hujson --json rc 0
{
  "decision": {
    "allowed": true,
    "reason": "owner",
    "rules": null
  }
}

PASS F18-04 the owner guard holds against any deny (personal draft)

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --json rc 1
{"schema_version":"1.0","ok":false,"error":{"code":"invalid_policy","category":"usage","message":"The policy file is not valid. Fix the listed errors and try again. Support reference: support_5233a12b-f44f-4646-bd26-422092b80f90.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_5233a12b-f44f-4646-bd26-422092b80f90"}}

PASS F18-04 what-can has no exec on the machine for the member

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --file /tmp/f18-draft.hujson --json rc 1
{"schema_version":"1.0","ok":false,"error":{"code":"invalid_policy","category":"usage","message":"The policy file is not valid. Fix the listed errors and try again. Support reference: support_7f88c077-c2a7-42a5-8271-f4aec3e31c5e.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_7f88c077-c2a7-42a5-8271-f4aec3e31c5e"}}

FAIL F18-04 explain on a draft without the deny allows the member: {}

COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes rc 1
pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_a4aacdf3-3b4d-4547-ba93-391835c1295a.

FAIL F18-08 a member cannot add a rule on another member's machine: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_a4aacdf3-3b4d-4547-ba93-391835c1295a.

COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json rc 1
{"schema_version":"1.0","ok":false,"error":{"code":"workspace_unavailable","category":"authorization_or_entitlement","message":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_95d893ce-0baa-472c-9f78-6793720d8011.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_95d893ce-0baa-472c-9f78-6793720d8011"}}

FAIL F18-08 a member explains their own access: rt reference: support_95d893ce-0baa-472c-9f78-6793720d8011.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_95d893ce-0baa-472c-9f78-6793720d8011"}}

COMMAND ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json rc 1
{"schema_version":"1.0","ok":false,"error":{"code":"workspace_unavailable","category":"authorization_or_entitlement","message":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_71a2fab2-645a-4afe-a992-dd7aeb506e55.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_71a2fab2-645a-4afe-a992-dd7aeb506e55"}}

FAIL F18-08 a member cannot explain someone else's access: ge":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_71a2fab2-645a-4afe-a992-dd7aeb506e55.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_71a2fab2-645a-4afe-a992-dd7aeb506e55"}}

COMMAND ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb policy restore 25 --workspace paperboat-smoke-365ac02bcda9 --message 'f18-team-check cleanup' --yes rc 0
Access changes:
+ gained user:<redacted-email> machine:lanex2-l exec (/rules/1)
Saved policy version 28 (sha256:8ac1743011622fdec22ad80039006a8e572c286f5e464274b05a21087e44f6b8).

PASS restore the starting policy version

```

## Direct member checks
```
xa2-l2 $ pb version 
rc 0 
       ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d24378a95
   ▀███████▀

xa2-l2 $ pb machine list --workspace paperboat-smoke-365ac02bcda9 --json 
rc 1 
 {"schema_version":"1.0","ok":false,"error":{"code":"workspace_unavailable","category":"authorization_or_entitlement","message":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_3bfcede5-5e6c-4f3a-a037-e4f69e3a4f1a.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_3bfcede5-5e6c-4f3a-a037-e4f69e3a4f1a"}}

xa2-l2 $ pb policy test --workspace paperboat-smoke-365ac02bcda9 
rc 1 
 pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_6c722e18-4f77-4ed2-a494-6433f57e5f35.

xa2-l2 $ pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<member> --dst machine:lanex2-l --action exec --json 
rc 1 
 {"schema_version":"1.0","ok":false,"error":{"code":"workspace_unavailable","category":"authorization_or_entitlement","message":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_c325f3c6-42ba-433e-9328-9ab35d5d7a98.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_c325f3c6-42ba-433e-9328-9ab35d5d7a98"}}

xa2-l2 $ pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<member> --dst machine:lanex2-w --action exec --json 
rc 1 
 {"schema_version":"1.0","ok":false,"error":{"code":"workspace_unavailable","category":"authorization_or_entitlement","message":"You do not have permission to perform this action. Check the selected account and target. Support reference: support_78b956f1-1ee4-4bb5-93a9-44bbbf803e0c.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_78b956f1-1ee4-4bb5-93a9-44bbbf803e0c"}}

xa2-l2 $ pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo X-34-marker 
rc 1 
 pb: You do not have permission to perform this action. Check the selected account and target. Support reference: support_d6fc0624-4790-4c41-b9ad-44190671d88f.


```

The live refusal wording for both targets is the same apart from the unique support reference. Both explain calls return workspace_unavailable, with no deciding rule. `policy test` also fails on workspace access, so no allow/deny or rule equivalence is claimed. Windows was only named in a read-only member exec probe: it was not shared for a second policy run. Windows-member/client comparison was conditional on Linux passing and was not attempted; no winpool runner created.

## Read-only diagnosis and cleanup
Haiku diagnosis verified against server/internal/workspace/scope.go:118-125 (requires m.active) and server/internal/httpapi/workspace_scope.go:183 (ErrForbidden -> workspace_unavailable). This observed fixture failure does not demonstrate FIX-BT. Restoring an accepted active member requires a new invitation/acceptance; that account/team-setting change was not made.
Harness restored starting policy version 25, restored lanex2-l name, and unshared that machine successfully, as its cleanup contract specifies. Removed /tmp/f18-deny-all.hujson and /tmp/f18-draft.hujson from lanex2-l. No accounts, invitations, runners, machine records or credentials created; none revoked. No fleet installations or pairing changes. No Go changes or Go tests.
