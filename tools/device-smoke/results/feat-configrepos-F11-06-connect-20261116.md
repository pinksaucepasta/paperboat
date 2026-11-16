# F11-06 connect-time validation (FEAT-CONFIGREPOS), build dev-941936e87 (main 524f639a5)

Server: prod as of 2026-11-16 (connect check is client-side; the tag-rule part waits for the deploy of migrations 369-370).
Fixtures: public https://github.com/octocat/Spoon-Knife.git (valid, no manifest); local bare repo fcr-bad.git whose .paperboat/config-sync.toml is `mode = [`.

## L: pbvm2 container fcr-l (machine fcr-l-1116, machine_8bea3206-...)
```
Installed Paperboat dev-941936e87 at /home/pbt/.local/bin/pb. Background update checks: false. Enrollment is unchanged.
== valid-https rc=0
{"repository":{"state":"active","id":"repository_b668b0b5-d4fc-45a7-a6ec-91759d0d09d6","provider":"git","external_ref":"https://github.com/octocat/Spoon-Knife.git","display_name":"fcr-spoon"},"version":"1"}
== missing-branch rc=1
pb: The config repository does not have the sync branch. Create that branch (or pick the repository's existing branch in the machine configuration), then retry. Support reference: support_<ref>. Run `pb bugreport --upload` to send the details.
== not-found rc=2
pb: The repository was not found or refused these credentials. Check the URL, or pass --credentials-file <profile> (the JSON `pb config repository credentials set` reads) for a private repository.
== bad-manifest rc=2
pb: The config repository's .paperboat/config-sync.toml is not a valid config sync file (invalid config sync source configuration: toml: line 1 (last key "mode"): unexpected EOF; expected value). Fix it in the repository, then retry.
== ssh-no-creds rc=2
pb: Checking an SSH repository needs its credentials: pass --credentials-file <profile> (the JSON `pb config repository credentials set` reads).
== list rc=0
```

## W: winpool-5 (machine fcr-w-1116d, revoked after)
```

  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-941936e87
== valid-https rc=0
{"repository":{"state":"active","id":"repository_b668b0b5-d4fc-45a7-a6ec-91759d0d09d6","provider":"git","external_ref":"https://github.com/octocat/Spoon-Knife.git","display_name":"fcr-spoon"},"version":"1"}
== missing-branch rc=1
pb.exe : pb: The config repository does not have the sync branch. Create that branch (or pick the repository's 
existing branch in the machine configuration), then retry. Support reference: 
== not-found rc=2
pb.exe : pb: The repository was not found or refused these credentials. Check the URL, or pass --credentials-file 
<profile> (the JSON `pb config repository credentials set` reads) for a private repository.
== bad-manifest rc=2
pb.exe : pb: The config repository's .paperboat/config-sync.toml is not a valid config sync file (invalid config sync 
source configuration: toml: line 1 (last key "mode"): unexpected EOF; expected value). Fix it in the repository, then 
== ssh-no-creds rc=2
pb.exe : pb: Checking an SSH repository needs its credentials: pass --credentials-file <profile> (the JSON `pb config 
repository credentials set` reads).


```
