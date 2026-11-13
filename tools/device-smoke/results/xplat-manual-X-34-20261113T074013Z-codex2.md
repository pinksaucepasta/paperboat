# X-34 non-owner policy comparison

Member account already active at start; no invitation or membership was changed. Two invite attempts returned team_changed because the current xa2-l2 account is already active (a different inactive member remains).
Membership verified: [('user_551ab5fe-1a86-49c7-b525-59ddc4ffa0d5', 'member', False), ('user_75f557e8-3c6c-44aa-8b83-3090c75e839f', 'member', True), ('user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2', 'owner', True)]
Member transport: ssh <redacted-email> docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb
Member pb version:       ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d24378a95
   ▀███████▀


## lanex2-m
```
pb version
      ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d8c123c51
   ▀███████▀
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS owner reads the team policy
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine share paperboat-smoke-365ac02bcda9 machine_9e1851b7-5763-40ab-9834-720becc3eb6d --generation 213 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS share the owner machine into the team
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-m -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_e67aecb4-e673-4abf-944f-2c801161ef68. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS no rule: the member is refused
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine grant paperboat-smoke-365ac02bcda9 machine_9e1851b7-5763-40ab-9834-720becc3eb6d --all-members --capability exec --generation 214 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS simple view: grant exec to every member
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-m -- echo f18-team-check
rc=0
f18-team-check
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one allow exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS the member can exec
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS the team policy is plain JSON to extend
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --dry-run
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
```
PASS F18-05 the review lists the member losing exec, nothing saved
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=2
/tests/0/allow/0: test_failed: expected user:<redacted-email> to be allowed exec on machine:lanex2-m, but the policy says denied by /rules/5
Tests: 0 passed, 1 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
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
```
PASS F18-04 a failing in-file test blocks the save
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-m --action exec --json
rc=0
{
  "who_can": [
    {
      "subject": "user:<redacted-email>",
      "target": "machine:lanex2-m",
      "action": "exec",
      "reason": "rule",
      "rules": [
        "/rules/4"
      ]
    },
    {
      "subject": "user:<redacted-email>",
      "target": "machine:lanex2-m",
      "action": "exec",
      "reason": "rule",
      "rules": [
        "/rules/4"
      ]
    }
  ]
}
```
PASS F18-04 who-can lists the member while the grant allows it
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --message f18-team-check --yes
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-m exec (/rules/4)
Saved policy version 67 (sha256:c85dd341ab180fca42a27c3ae4897c04885bf5caff19ed461f611132579abd30).
```
PASS a deny rule is saved after the access review
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-m -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_701d3f60-97e9-48bf-aca7-3b8f20e31e62. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS deny wins over the grant: the member is refused
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS explain names the deny rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-m --action exec --json
rc=0
{}
```
PASS F18-04 who-can no longer lists the member
```
owner pb machine rename machine_9e1851b7-5763-40ab-9834-720becc3eb6d lanex2-mr
rc=0
Renamed machine lanex2-m to lanex2-mr (machine_9e1851b7-5763-40ab-9834-720becc3eb6d).
Machine: lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d)
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 a renamed machine reads with its new name in the policy
```
owner pb machine rename machine_9e1851b7-5763-40ab-9834-720becc3eb6d lanex2-m
rc=0
Renamed machine lanex2-mr to lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d).
Machine: lanex2-mr (machine_9e1851b7-5763-40ab-9834-720becc3eb6d)
```
```
owner pb policy explain --workspace personal --src user:<redacted-email> --dst machine:lanex2-m --action exec --file /Users/admin/x34-f18-deny-all.hujson --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "owner",
    "rules": null
  }
}
```
PASS F18-04 the owner guard holds against any deny (personal draft)
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --json
rc=0
{}
```
PASS F18-04 what-can has no exec on the machine for the member
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --file /Users/admin/x34-f18-draft.hujson --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS F18-04 explain on a draft without the deny allows the member
```
member pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
{
  "workspace": "paperboat-smoke-365ac02bcda9",
  "version": 67,
  "policy": "{\n  \"rules\": [\n    {\n      \"src\": [\n        \"role:admin\"\n      ],\n      \"dst\": [\n        \"env:team\"\n      ],\n      \"allow\": [\n        \"env_read\",\n        \"env_write\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-l\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-m-5\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-w\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"role:member\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-m\"\n      ],\n      \"allow\": [\n        \"exec\"\n      ]\n    },\n    {\n      \"src\": [\n        \"*\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-m\"\n      ],\n      \"deny\": [\n        \"exec\"\n      ],\n      \"note\": \"f18-team-check\"\n    }\n  ]\n}\n",
  "digest": "sha256:c85dd341ab180fca42a27c3ae4897c04885bf5caff19ed461f611132579abd30",
  "updated_at": "2026-11-13T07:45:00.144792Z",
  "updated_by": "user:<redacted-email>",
  "message": "f18-team-check",
  "can_edit": "none",
  "names": {
    "users": [
      "<redacted-email>",
      "<redacted-email>"
    ],
    "machines": [
      "xd27-w",
      "xd27-l",
      "lanex2-m",
      "xd27-m-5"
    ],
    "previews": [],
    "tunnels": [],
    "tags": [],
    "groups": []
  }
}
```
PASS F18-08 a member reads the policy and cannot edit everything
```
member pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=1
Tests: 0 passed, 0 failed
Access changes:
+ gained user:<redacted-email> machine:lanex2-m exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-m terminal (/rules/5)
+ gained user:<redacted-email> machine:lanex2-m exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-m terminal (/rules/5)
pb: You cannot save this policy change. Owners and admins edit the policy; members may only change rules for their own machines. Support reference: support_76d7b038-8c9a-459e-a295-8251270ebcc1.
```
PASS F18-08 a member cannot add a rule on another member's machine
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS F18-08 a member explains their own access
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-m --action exec --json
rc=1
{"schema_version":"1.0","ok":false,"error":{"code":"policy_explain_forbidden","category":"authorization_or_entitlement","message":"Only team owners and admins can explain another member's access. Support reference: support_c35e20ae-6639-498a-aedd-eda7066ee956.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_c35e20ae-6639-498a-aedd-eda7066ee956"}}
```
PASS F18-08 a member cannot explain someone else's access
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --generation 217 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":219,"deleted":false,"member_count":3,"grant_count":1,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":68,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":68,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":68,"active":true},{"machine_id":"machine_9e1851b7-5763-40ab-9834-720becc3eb6d","audience":"all_members","capabilities":["exec"],"generation":68,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 pb team grant gives the member the team ENV (team ENV must exist)
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/6"
    ],
    "key_delivered": false
  }
}
```
PASS F18-07 the grant is the member's rule and explain allows env_read
PASS F18-07 explain says the member has no team key yet
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --active=false --generation 219 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":221,"deleted":false,"member_count":3,"grant_count":0,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":69,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":69,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":69,"active":true},{"machine_id":"machine_9e1851b7-5763-40ab-9834-720becc3eb6d","audience":"all_members","capabilities":["exec"],"generation":69,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 revoking the grant removes the rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS F18-07 after the revoke explain denies env_read
```
owner pb policy restore 65 --workspace paperboat-smoke-365ac02bcda9 --message 'f18-team-check cleanup' --yes
rc=0
Saved policy version 70 (sha256:aef155ae7730dbe740ae90efadb1ce3536786413a14c24223af75ebb5af51ffa).
```
PASS restore the starting policy version
```
owner pb team activity paperboat-smoke-365ac02bcda9 --action team.policy --since 1h --json
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 saves and restores are audit events
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine unshare paperboat-smoke-365ac02bcda9 machine_9e1851b7-5763-40ab-9834-720becc3eb6d --generation 222 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS unshare the machine
RESULT PASS
Harness exit 0

## lanex2-w
```
pb version
      ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d8c123c51
   ▀███████▀
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS owner reads the team policy
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine share paperboat-smoke-365ac02bcda9 machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1 --generation 223 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS share the owner machine into the team
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_c51987e9-3deb-4787-b103-453bb0b05e9b. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS no rule: the member is refused
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine grant paperboat-smoke-365ac02bcda9 machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1 --all-members --capability exec --generation 224 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS simple view: grant exec to every member
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo f18-team-check
rc=1
pb: The target machine was reached but did not open the connection. Its Paperboat service may still be starting, or it has not accepted this machine yet. Retry in a minute; run `pb doctor` if this continues. Support reference: support_0c13f828-3c3f-4a9a-9cbc-2af25477ddbc. Run `pb bugreport --upload` to send the details.
```
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo f18-team-check
rc=1
pb: The target machine was reached but did not open the connection. Its Paperboat service may still be starting, or it has not accepted this machine yet. Retry in a minute; run `pb doctor` if this continues. Support reference: support_5e02ea42-f5e4-4463-8c1f-5a35de950fa6. Run `pb bugreport --upload` to send the details.
```
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo f18-team-check
rc=0
f18-team-check
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one allow exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS the member can exec
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS the team policy is plain JSON to extend
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --dry-run
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
```
PASS F18-05 the review lists the member losing exec, nothing saved
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=2
/tests/0/allow/0: test_failed: expected user:<redacted-email> to be allowed exec on machine:lanex2-w, but the policy says denied by /rules/5
Tests: 0 passed, 1 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
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
```
PASS F18-04 a failing in-file test blocks the save
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-w --action exec --json
rc=0
{
  "who_can": [
    {
      "subject": "user:<redacted-email>",
      "target": "machine:lanex2-w",
      "action": "exec",
      "reason": "rule",
      "rules": [
        "/rules/4"
      ]
    },
    {
      "subject": "user:<redacted-email>",
      "target": "machine:lanex2-w",
      "action": "exec",
      "reason": "rule",
      "rules": [
        "/rules/4"
      ]
    }
  ]
}
```
PASS F18-04 who-can lists the member while the grant allows it
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --message f18-team-check --yes
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-w exec (/rules/4)
Saved policy version 72 (sha256:a7c4d7c3fa385186367cc17dd20ad4e3a9de7fdd03d7554a9040c39053cc0bb6).
```
PASS a deny rule is saved after the access review
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-w -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_4b95060b-d14a-4183-83f9-53311e08dd90. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS deny wins over the grant: the member is refused
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS explain names the deny rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-w --action exec --json
rc=0
{}
```
PASS F18-04 who-can no longer lists the member
```
owner pb machine rename machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1 lanex2-wr
rc=0
Renamed machine lanex2-w to lanex2-wr (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1).
Machine: lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 a renamed machine reads with its new name in the policy
```
owner pb machine rename machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1 lanex2-w
rc=0
Renamed machine lanex2-wr to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1).
Machine: lanex2-wr (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
```
```
owner pb policy explain --workspace personal --src user:<redacted-email> --dst machine:lanex2-w --action exec --file 'C:\Users\Pujan\x34-f18-deny-all.hujson' --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "owner",
    "rules": null
  }
}
```
PASS F18-04 the owner guard holds against any deny (personal draft)
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --json
rc=0
{}
```
PASS F18-04 what-can has no exec on the machine for the member
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --file 'C:\Users\Pujan\x34-f18-draft.hujson' --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS F18-04 explain on a draft without the deny allows the member
```
member pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
{
  "workspace": "paperboat-smoke-365ac02bcda9",
  "version": 72,
  "policy": "{\n  \"rules\": [\n    {\n      \"src\": [\n        \"role:admin\"\n      ],\n      \"dst\": [\n        \"env:team\"\n      ],\n      \"allow\": [\n        \"env_read\",\n        \"env_write\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-l\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-m-5\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-w\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"role:member\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-w\"\n      ],\n      \"allow\": [\n        \"exec\"\n      ]\n    },\n    {\n      \"src\": [\n        \"*\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-w\"\n      ],\n      \"deny\": [\n        \"exec\"\n      ],\n      \"note\": \"f18-team-check\"\n    }\n  ]\n}\n",
  "digest": "sha256:a7c4d7c3fa385186367cc17dd20ad4e3a9de7fdd03d7554a9040c39053cc0bb6",
  "updated_at": "2026-11-13T07:48:10.439373Z",
  "updated_by": "user:<redacted-email>",
  "message": "f18-team-check",
  "can_edit": "none",
  "names": {
    "users": [
      "<redacted-email>",
      "<redacted-email>"
    ],
    "machines": [
      "lanex2-w",
      "xd27-w",
      "xd27-l",
      "xd27-m-5"
    ],
    "previews": [],
    "tunnels": [],
    "tags": [],
    "groups": []
  }
}
```
PASS F18-08 a member reads the policy and cannot edit everything
```
member pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=1
Tests: 0 passed, 0 failed
Access changes:
+ gained user:<redacted-email> machine:lanex2-w exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-w terminal (/rules/5)
+ gained user:<redacted-email> machine:lanex2-w exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-w terminal (/rules/5)
pb: You cannot save this policy change. Owners and admins edit the policy; members may only change rules for their own machines. Support reference: support_cb24a270-5aa1-4dff-8ffe-d723e0286a2d.
```
PASS F18-08 a member cannot add a rule on another member's machine
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS F18-08 a member explains their own access
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-w --action exec --json
rc=1
{"schema_version":"1.0","ok":false,"error":{"code":"policy_explain_forbidden","category":"authorization_or_entitlement","message":"Only team owners and admins can explain another member's access. Support reference: support_6f344bf8-c298-40cc-a321-15a98b063a15.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_6f344bf8-c298-40cc-a321-15a98b063a15"}}
```
PASS F18-08 a member cannot explain someone else's access
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --generation 228 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":230,"deleted":false,"member_count":3,"grant_count":1,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":73,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":73,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":73,"active":true},{"machine_id":"machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1","audience":"all_members","capabilities":["exec"],"generation":73,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 pb team grant gives the member the team ENV (team ENV must exist)
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/6"
    ],
    "key_delivered": false
  }
}
```
PASS F18-07 the grant is the member's rule and explain allows env_read
PASS F18-07 explain says the member has no team key yet
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --active=false --generation 230 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":232,"deleted":false,"member_count":3,"grant_count":0,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":74,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":74,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":74,"active":true},{"machine_id":"machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1","audience":"all_members","capabilities":["exec"],"generation":74,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 revoking the grant removes the rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS F18-07 after the revoke explain denies env_read
```
owner pb policy restore 70 --workspace paperboat-smoke-365ac02bcda9 --message 'f18-team-check cleanup' --yes
rc=0
Saved policy version 75 (sha256:aef155ae7730dbe740ae90efadb1ce3536786413a14c24223af75ebb5af51ffa).
```
PASS restore the starting policy version
```
owner pb team activity paperboat-smoke-365ac02bcda9 --action team.policy --since 1h --json
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 saves and restores are audit events
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine unshare paperboat-smoke-365ac02bcda9 machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1 --generation 233 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS unshare the machine
RESULT PASS
Harness exit 0

## lanex2-l
```
pb version
      ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d8c123c51
   ▀███████▀
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS owner reads the team policy
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine share paperboat-smoke-365ac02bcda9 machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a --generation 234 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS share the owner machine into the team
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_ca71c7c8-0cc6-4806-b00f-3705547264e3. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS no rule: the member is refused
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine grant paperboat-smoke-365ac02bcda9 machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a --all-members --capability exec --generation 235 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS simple view: grant exec to every member
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check
rc=0
f18-team-check
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one allow exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS the member can exec
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS the team policy is plain JSON to extend
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --dry-run
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
```
PASS F18-05 the review lists the member losing exec, nothing saved
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=2
/tests/0/allow/0: test_failed: expected user:<redacted-email> to be allowed exec on machine:lanex2-l, but the policy says denied by /rules/5
Tests: 0 passed, 1 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
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
```
PASS F18-04 a failing in-file test blocks the save
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-l --action exec --json
rc=0
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
    },
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
```
PASS F18-04 who-can lists the member while the grant allows it
```
owner pb policy set - --workspace paperboat-smoke-365ac02bcda9 --message f18-team-check --yes
rc=0
Tests: 0 passed, 0 failed
Access changes:
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
- lost user:<redacted-email> machine:lanex2-l exec (/rules/4)
Saved policy version 77 (sha256:c1d9f3781c231f2d234e4023543c1b0a73259062560235ebdb5fa8d734b6f806).
```
PASS a deny rule is saved after the access review
```
member pb exec --workspace paperboat-smoke-365ac02bcda9 lanex2-l -- echo f18-team-check
rc=1
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_c191cf52-079c-4100-b12a-81953cecc0d2. Run `pb bugreport --upload` to send the details.
```
```
owner pb policy test - --workspace paperboat-smoke-365ac02bcda9
stdin: saved rules + one deny exec test for member and target
rc=0
Tests: 1 passed, 0 failed
No access changes.
```
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS deny wins over the grant: the member is refused
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS explain names the deny rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --dst machine:lanex2-l --action exec --json
rc=0
{}
```
PASS F18-04 who-can no longer lists the member
```
owner pb machine rename machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a lanex2-lr
rc=0
Renamed machine lanex2-l to lanex2-lr (machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a).
Machine: lanex2-l (machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a)
```
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 a renamed machine reads with its new name in the policy
```
owner pb machine rename machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a lanex2-l
rc=0
Renamed machine lanex2-lr to lanex2-l (machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a).
Machine: lanex2-lr (machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a)
```
```
owner pb policy explain --workspace personal --src user:<redacted-email> --dst machine:lanex2-l --action exec --file /home/tester/x34-f18-deny-all.hujson --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "owner",
    "rules": null
  }
}
```
PASS F18-04 the owner guard holds against any deny (personal draft)
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --json
rc=0
{}
```
PASS F18-04 what-can has no exec on the machine for the member
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --file /home/tester/x34-f18-draft.hujson --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/4"
    ]
  }
}
```
PASS F18-04 explain on a draft without the deny allows the member
```
member pb policy get --workspace paperboat-smoke-365ac02bcda9 --json
rc=0
{
  "workspace": "paperboat-smoke-365ac02bcda9",
  "version": 77,
  "policy": "{\n  \"rules\": [\n    {\n      \"src\": [\n        \"role:admin\"\n      ],\n      \"dst\": [\n        \"env:team\"\n      ],\n      \"allow\": [\n        \"env_read\",\n        \"env_write\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-l\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-m-5\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"user:<redacted-email>\"\n      ],\n      \"dst\": [\n        \"machine:xd27-w\"\n      ],\n      \"allow\": [\n        \"exec\",\n        \"files\",\n        \"terminal\"\n      ]\n    },\n    {\n      \"src\": [\n        \"role:member\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-l\"\n      ],\n      \"allow\": [\n        \"exec\"\n      ]\n    },\n    {\n      \"src\": [\n        \"*\"\n      ],\n      \"dst\": [\n        \"machine:lanex2-l\"\n      ],\n      \"deny\": [\n        \"exec\"\n      ],\n      \"note\": \"f18-team-check\"\n    }\n  ]\n}\n",
  "digest": "sha256:c1d9f3781c231f2d234e4023543c1b0a73259062560235ebdb5fa8d734b6f806",
  "updated_at": "2026-11-13T07:50:02.431832Z",
  "updated_by": "user:<redacted-email>",
  "message": "f18-team-check",
  "can_edit": "none",
  "names": {
    "users": [
      "<redacted-email>",
      "<redacted-email>"
    ],
    "machines": [
      "xd27-w",
      "lanex2-l",
      "xd27-l",
      "xd27-m-5"
    ],
    "previews": [],
    "tunnels": [],
    "tags": [],
    "groups": []
  }
}
```
PASS F18-08 a member reads the policy and cannot edit everything
```
member pb policy set - --workspace paperboat-smoke-365ac02bcda9 --yes
rc=1
Tests: 0 passed, 0 failed
Access changes:
+ gained user:<redacted-email> machine:lanex2-l exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-l terminal (/rules/5)
+ gained user:<redacted-email> machine:lanex2-l exec (/rules/4)
+ gained user:<redacted-email> machine:lanex2-l terminal (/rules/5)
pb: You cannot save this policy change. Owners and admins edit the policy; members may only change rules for their own machines. Support reference: support_211332a2-505c-4dea-99f2-215655696ee2.
```
PASS F18-08 a member cannot add a rule on another member's machine
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "denied_by_rule",
    "rules": [
      "/rules/5"
    ]
  }
}
```
PASS F18-08 a member explains their own access
```
member pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst machine:lanex2-l --action exec --json
rc=1
{"schema_version":"1.0","ok":false,"error":{"code":"policy_explain_forbidden","category":"authorization_or_entitlement","message":"Only team owners and admins can explain another member's access. Support reference: support_29c80f07-0211-4a3a-9960-724ab4522238.","retryable":false,"state_changed":"unknown","outcome_uncertain":false,"support_reference":"support_29c80f07-0211-4a3a-9960-724ab4522238"}}
```
PASS F18-08 a member cannot explain someone else's access
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --generation 238 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":240,"deleted":false,"member_count":3,"grant_count":1,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":78,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":78,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":78,"active":true},{"machine_id":"machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a","audience":"all_members","capabilities":["exec"],"generation":78,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 pb team grant gives the member the team ENV (team ENV must exist)
```
owner pb policy get --workspace paperboat-smoke-365ac02bcda9
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": true,
    "reason": "rule",
    "rules": [
      "/rules/6"
    ],
    "key_delivered": false
  }
}
```
PASS F18-07 the grant is the member's rule and explain allows env_read
PASS F18-07 explain says the member has no team key yet
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team grant paperboat-smoke-365ac02bcda9 user_75f557e8-3c6c-44aa-8b83-3090c75e839f env paperboat-smoke-365ac02bcda9 read --active=false --generation 240 --json
rc=0
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"terminal_session_grants":[],"env_status":"rotation_pending","team_id":"paperboat-smoke-365ac02bcda9","owner_account":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","generation":242,"deleted":false,"member_count":3,"grant_count":0,"machine_count":4,"my_role":"owner","machine_grants":[{"machine_id":"machine_92aa8b30-415c-441c-993b-83837bdf418d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":79,"active":true},{"machine_id":"machine_a3e1923d-5f82-4079-b2df-9fd356c0ca7d","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":79,"active":true},{"machine_id":"machine_5338215f-159c-411f-bf46-0ccc3aa65af0","audience":"selected_member","account_id":"user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2","capabilities":["exec","files","terminal"],"generation":79,"active":true},{"machine_id":"machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a","audience":"all_members","capabilities":["exec"],"generation":79,"active":true}],"approve_new_machines":true,"machine_requests":[],"allow_private_access":false,"env_names_listable":true}
```
PASS F18-07 revoking the grant removes the rule
```
owner pb policy explain --workspace paperboat-smoke-365ac02bcda9 --src user:<redacted-email> --dst env:team --action env_read --json
rc=0
{
  "decision": {
    "allowed": false,
    "reason": "no_rule",
    "rules": null
  }
}
```
PASS F18-07 after the revoke explain denies env_read
```
owner pb policy restore 75 --workspace paperboat-smoke-365ac02bcda9 --message 'f18-team-check cleanup' --yes
rc=0
Saved policy version 80 (sha256:aef155ae7730dbe740ae90efadb1ce3536786413a14c24223af75ebb5af51ffa).
```
PASS restore the starting policy version
```
owner pb team activity paperboat-smoke-365ac02bcda9 --action team.policy --since 1h --json
rc=0
[successful response omitted; harness assertions below]
```
PASS F18-05 saves and restores are audit events
```
owner pb team get paperboat-smoke-365ac02bcda9 --json
rc=0
[successful response omitted; harness assertions below]
```
```
owner pb team machine unshare paperboat-smoke-365ac02bcda9 machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a --generation 243 --json
rc=0
[successful response omitted; harness assertions below]
```
PASS unshare the machine
RESULT PASS
Harness exit 0

No installs, pairing, membership changes or machine revocations. Harness restores policy and unshares each target. CHECKLIST unchanged. Optional Mac/Windows member client not checked. No Go code changed; no Go build/vet/test run.

## Comparison and scope

Final rerun: policy_team.py RESULT PASS on M, W, L; each restored its starting policy and unshared its target. The initial L/M attempt stopped at an input-encoding error in the recording wrapper, with cleanup; corrected to bytes and rerun. Windows briefly refused a connection after grant, then succeeded within the harness propagation retries. Linux container was briefly paused by another session between runs; no fleet state was changed to recover it.

The owner and member command prefixes above identify where each pb command ran. The existing policy_team.py body was used unchanged; only transport/recording wrappers and extra policy test/explain probes were added in a temporary local driver. Test stdin was the saved policy with a single src=the active member, dst=machine:<target>, allow/deny=[exec] assertion.

| Target | Build | Allow explain | Deny explain | Policy tests | Live exec |
|---|---|---|---|---|---|
| lanex2-l | dev-d8c123c51 | allowed=true, reason=rule, /rules/4 | allowed=false, reason=denied_by_rule, /rules/5 | no-rule/allow/deny: each 1 passed, 0 failed | allow marker; deny refused |
| lanex2-m | dev-d8c123c51 | allowed=true, reason=rule, /rules/4 | allowed=false, reason=denied_by_rule, /rules/5 | no-rule/allow/deny: each 1 passed, 0 failed | allow marker; deny refused |
| lanex2-w | dev-d8c123c51 | allowed=true, reason=rule, /rules/4 | allowed=false, reason=denied_by_rule, /rules/5 | no-rule/allow/deny: each 1 passed, 0 failed | allow marker; deny refused |

`policy test` reports test counts, not rule pointers; the matching explain probes above supply the deciding rule. All explicit-deny live refusals were `pb: No machine has that name or ID. Run pb machine list to see your machines.` followed by a unique support reference and the same bugreport advice (exact text in command outputs above). This is policy-filtered inventory refusal, rather than a host-side message naming the deny rule.

Owner transport mapping for every recorded command:
- L: `ssh hp docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb`.
- M: `~/.pbmac/ssh.sh vm` running `/Users/admin/.local/bin/pb` through `xplat_fleet.mac()` (keychain unlock handled privately by that existing adapter).
- W: `xplat_fleet.windows()` / `winhost.WindowsDevice.run`, Victus, discovered per-user installed pb.exe. Uses argv directly, no shell interpolation.
- Member: `ssh ubuntu@anvit docker exec -i -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 xa2-l2 /home/tester/.local/bin/pb`.

The member is user_75f557e8-3c6c-44aa-8b83-3090c75e839f, role member, active=true. Its existing machine/build were reused; the older member build is an explicit limitation. No throwaway machine records were created, so none needed revoking. No failed product check remained after retries/reruns. No checklist ticks.
