# F21-11 M: ENV wording while Paperboat is unreachable (macOS 26.7.2 arm64, macpool-5 relout-mac5, pb dev-d77a40713, staging)

API TCP blocked on the Mac by the drill's pf anchor (control_outage_mac.MacEnv.cut), 110 s into the cut:

    $ pb status
    Daemon: degraded  Generation: 31
    Health: error: Control plane is unavailable  Recovery: Check network access and Paperboat authentication
    $ pb env list        (this computer has no ENV vault)
    pb: ENV needs Paperboat online: this computer has no ENV vault yet and Paperboat is unreachable, so it cannot tell whether this account uses ENV. Nothing was changed; run it again once `pb status` shows Paperboat is reachable. ...

With the control plane reachable the same command says "ENV is not set up for this account on this computer".
Every offline `pb exec` in control-outage-mac-20261113T152223Z.json printed: "Paperboat ENV not applied: ENV needs Paperboat online. The command ran without your managed values; run it again once `pb status` shows Paperboat is reachable."
Unit tests: TestEnvWithoutAVaultWhileOfflineSaysItNeedsPaperboatOnline, TestPendingEnvironmentLaunchAlsoSaysEnvNeedsPaperboatOnline, TestLaunchNoticeForNamesWhyDeliveryFailed (CI run 43423162879, commit f320c4b8a).
