# pb command reference

Generated from the public CLI command definitions. Do not edit individual pages; run `make cli-docs` after updating command help.

Read the [CLI guide](../cli.md) for interactive use, scripting, installation of man pages, and recovery.

| Command | Description |
| --- | --- |
| [pb](pb.md) | Open Paperboat or connect to an environment terminal |
| [pb access](pb_access.md) | Open authenticated private access |
| [pb access device](pb_access_device.md) | Forward a local port to an authorized device service |
| [pb access tunnel](pb_access_tunnel.md) | Open private TCP access through stable hostd |
| [pb approve](pb_approve.md) | Approve or revoke a peer device over gRPC IPC |
| [pb auth](pb_auth.md) | Manage Paperboat sign-in |
| [pb auth login](pb_auth_login.md) | Sign in with a 26-character enrollment token |
| [pb auth logout](pb_auth_logout.md) | Revoke and remove the active client session |
| [pb auth status](pb_auth_status.md) | Show the active Paperboat account |
| [pb auth switch](pb_auth_switch.md) | Show dashboard enrollment instructions |
| [pb bugreport](pb_bugreport.md) | Create a redacted Paperboat diagnostic bundle |
| [pb completion](pb_completion.md) | Generate the autocompletion script for the specified shell |
| [pb completion bash](pb_completion_bash.md) | Generate the autocompletion script for bash |
| [pb completion fish](pb_completion_fish.md) | Generate the autocompletion script for fish |
| [pb completion powershell](pb_completion_powershell.md) | Generate the autocompletion script for powershell |
| [pb completion zsh](pb_completion_zsh.md) | Generate the autocompletion script for zsh |
| [pb config](pb_config.md) | Inspect the local CLI config |
| [pb config approve](pb_config_approve.md) | Approve the currently reviewed pull revision |
| [pb config assign](pb_config_assign.md) | Assign a config repository to a machine |
| [pb config conflict](pb_config_conflict.md) | Inspect and resolve configuration conflicts |
| [pb config conflict list](pb_config_conflict_list.md) | List current path conflicts |
| [pb config conflict resolve](pb_config_conflict_resolve.md) | Choose the machine or repository version |
| [pb config conflict show](pb_config_conflict_show.md) | Show a current path conflict |
| [pb config customize](pb_config_customize.md) | Customize local shortcuts, command defaults, and TUI appearance |
| [pb config customize explain](pb_config_customize_explain.md) | Show command expansion without executing it |
| [pb config customize import](pb_config_customize_import.md) | Validate and replace local preferences from a JSON file |
| [pb config customize path](pb_config_customize_path.md) | Print the local preference file path |
| [pb config customize reset](pb_config_customize_reset.md) | Reset only local CLI preferences; keep account and connection settings |
| [pb config customize show](pb_config_customize_show.md) | Show the local preference document |
| [pb config customize validate](pb_config_customize_validate.md) | Validate preferences without executing any action |
| [pb config force](pb_config_force.md) | Force a scoped configuration direction |
| [pb config path](pb_config_path.md) | Print the config file path |
| [pb config set](pb_config_set.md) | Set a local configuration value |
| [pb config show](pb_config_show.md) | Print the effective config |
| [pb config status](pb_config_status.md) | Show configuration synchronization status |
| [pb config status-bar](pb_config_status-bar.md) | Configure the interactive terminal status bar |
| [pb config status-bar preview](pb_config_status-bar_preview.md) | Preview the configured status bar |
| [pb config status-bar reset](pb_config_status-bar_reset.md) | Restore status-bar defaults |
| [pb config status-bar set](pb_config_status-bar_set.md) | Set a status-bar preference |
| [pb config status-bar show](pb_config_status-bar_show.md) | Show the effective status-bar configuration |
| [pb config team-default-adopt](pb_config_team-default-adopt.md) | Adopt a team default using your provider access |
| [pb config team-default-set](pb_config_team-default-set.md) | Set a team's default pull repository |
| [pb config team-default-unadopt](pb_config_team-default-unadopt.md) | Stop inheriting a team configuration default |
| [pb config unassign](pb_config_unassign.md) | Remove a config repository assignment |
| [pb config unset](pb_config_unset.md) | Remove a local configuration value |
| [pb connect](pb_connect.md) | Create and attach to an environment terminal session |
| [pb daemon](pb_daemon.md) | Paperboat endpoint daemon |
| [pb daemon device-guard](pb_daemon_device-guard.md) | Manage protected device-name access |
| [pb daemon device-guard install](pb_daemon_device-guard_install.md) | Install the root-owned device guard service |
| [pb daemon device-guard run](pb_daemon_device-guard_run.md) |  |
| [pb daemon device-guard uninstall](pb_daemon_device-guard_uninstall.md) | Remove device access while retaining cached-address protection |
| [pb daemon run](pb_daemon_run.md) | Run Paperboat daemon under service supervision |
| [pb daemon service](pb_daemon_service.md) | Manage the Paperboat background daemon service |
| [pb daemon service install](pb_daemon_service_install.md) | Install Paperboat daemon as the current-user service |
| [pb daemon service restart](pb_daemon_service_restart.md) | Restart Paperboat daemon current-user service |
| [pb daemon service start](pb_daemon_service_start.md) | Start Paperboat daemon current-user service |
| [pb daemon service status](pb_daemon_service_status.md) | Show Paperboat daemon current-user service status |
| [pb daemon service stop](pb_daemon_service_stop.md) | Stop Paperboat daemon current-user service |
| [pb daemon service uninstall](pb_daemon_service_uninstall.md) | Uninstall Paperboat daemon current-user service |
| [pb desktop](pb_desktop.md) | Authenticated desktop management bridge |
| [pb desktop request](pb_desktop_request.md) |  |
| [pb doctor](pb_doctor.md) | Check Paperboat connectivity and readiness |
| [pb env](pb_env.md) | Manage ENV Injection for connected hosts |
| [pb env grants](pb_env_grants.md) | Reconcile encrypted ENV team grants |
| [pb env grants sync](pb_env_grants_sync.md) | Accept pending team grants into this account's encrypted vault |
| [pb env host](pb_env_host.md) | Manage encrypted host ENV projections |
| [pb env host provision](pb_env_host_provision.md) | Provision an explicit encrypted ENV selection to a host |
| [pb env list](pb_env_list.md) | List configured environment-variable metadata |
| [pb env rotate](pb_env_rotate.md) | Rotate personal ENV scope keys while preserving values |
| [pb env rotate cancel](pb_env_rotate_cancel.md) | Cancel an uncommitted personal ENV key rotation |
| [pb env set](pb_env_set.md) | Set one environment variable through a hidden prompt or bounded stdin |
| [pb env team](pb_env_team.md) | Manage encrypted ENV team scopes |
| [pb env team create](pb_env_team_create.md) | Create an encrypted ENV team scope |
| [pb env team grant](pb_env_team_grant.md) | Grant an account access to an encrypted ENV team scope |
| [pb env team reset](pb_env_team_reset.md) | Discard all values and replace an encrypted ENV team key |
| [pb env team revoke](pb_env_team_revoke.md) | Rotate an ENV team scope and revoke selected members |
| [pb env team rotate](pb_env_team_rotate.md) | Rotate an encrypted ENV team key while preserving its values |
| [pb env unset](pb_env_unset.md) | Remove one environment variable |
| [pb env vault](pb_env_vault.md) | Manage password-protected ENV key custody |
| [pb env vault init](pb_env_vault_init.md) | Create a password-protected ENV vault |
| [pb env vault lock](pb_env_vault_lock.md) | Clear unlocked vault keys while retaining encrypted custody |
| [pb env vault password](pb_env_vault_password.md) | Rewrap unlocked ENV keys with a new password |
| [pb env vault recover](pb_env_vault_recover.md) | Recover vault access and replace the password and recovery code |
| [pb env vault recovery](pb_env_vault_recovery.md) | Enable, replace, or disable the optional recovery code |
| [pb env vault remove](pb_env_vault_remove.md) | Remove this device's local ENV vault custody |
| [pb env vault reset](pb_env_vault_reset.md) | Replace personal ENV keys and delete every personal value |
| [pb env vault resume](pb_env_vault_resume.md) | Reconcile an interrupted vault publication |
| [pb env vault unlock](pb_env_vault_unlock.md) | Unlock this account's ENV vault on this device |
| [pb environments](pb_environments.md) | List machines available to this account |
| [pb exec](pb_exec.md) | Execute an exact command on a machine |
| [pb inbox](pb_inbox.md) | Manage the Paperboat Inbox |
| [pb inbox approve](pb_inbox_approve.md) | Approve an exact team file request |
| [pb inbox decline](pb_inbox_decline.md) | Decline an exact team file request |
| [pb inbox path](pb_inbox_path.md) |  |
| [pb inbox policy](pb_inbox_policy.md) | Show or update team file acceptance |
| [pb inbox requests](pb_inbox_requests.md) | List team file requests |
| [pb inbox reset](pb_inbox_reset.md) |  |
| [pb inbox set](pb_inbox_set.md) |  |
| [pb install](pb_install.md) | Install this executable and its local service |
| [pb login](pb_login.md) | Show dashboard enrollment instructions |
| [pb logout](pb_logout.md) | Revoke and remove the active client session |
| [pb machine](pb_machine.md) | Manage machines |
| [pb machine add](pb_machine_add.md) | Print Linux/macOS and Windows machine enrollment commands |
| [pb machine availability](pb_machine_availability.md) | Set machine sleep availability |
| [pb machine capabilities](pb_machine_capabilities.md) | Set incoming services for a device |
| [pb machine list](pb_machine_list.md) | List enrolled machines |
| [pb machine rename](pb_machine_rename.md) | Rename a machine |
| [pb machine revoke](pb_machine_revoke.md) | Disconnect and revoke a machine |
| [pb pair](pb_pair.md) | Enroll this device with a one-shot token |
| [pb ping](pb_ping.md) | Measure authenticated connectivity to a machine |
| [pb preview](pb_preview.md) | Expose a local target through a temporary preview |
| [pb preview delete](pb_preview_delete.md) | Delete a temporary preview |
| [pb preview inspect](pb_preview_inspect.md) | Show daemon-local HTTP captures |
| [pb preview list](pb_preview_list.md) | List temporary previews |
| [pb preview replay](pb_preview_replay.md) | Deliberately replay one retained HTTP request to the same origin |
| [pb preview status](pb_preview_status.md) | Show temporary preview status |
| [pb preview stop](pb_preview_stop.md) | Stop a temporary preview |
| [pb relay](pb_relay.md) | Inspect Paperboat relays |
| [pb relay list](pb_relay_list.md) | List relays and measure current latency |
| [pb reset](pb_reset.md) | Remove the current Paperboat setup before fresh enrollment |
| [pb resolve](pb_resolve.md) | Resolve a peer device IP, port forwardings and tags over gRPC IPC |
| [pb rsync](pb_rsync.md) | Run rsync with Paperboat machine resolution |
| [pb scp](pb_scp.md) | Run scp with Paperboat machine resolution |
| [pb send](pb_send.md) | Send files to a machine's Paperboat Inbox |
| [pb service](pb_service.md) | Manage the Paperboat background daemon service |
| [pb service install](pb_service_install.md) | Install Paperboat daemon as the current-user service |
| [pb service restart](pb_service_restart.md) | Restart Paperboat daemon current-user service |
| [pb service start](pb_service_start.md) | Start Paperboat daemon current-user service |
| [pb service status](pb_service_status.md) | Show Paperboat daemon current-user service status |
| [pb service stop](pb_service_stop.md) | Stop Paperboat daemon current-user service |
| [pb service uninstall](pb_service_uninstall.md) | Uninstall Paperboat daemon current-user service |
| [pb session](pb_session.md) | Manage environment terminal sessions |
| [pb session attach](pb_session_attach.md) | Choose and attach to a durable terminal session |
| [pb session close](pb_session_close.md) | Close one or all terminal sessions |
| [pb session delete](pb_session_delete.md) | Delete a terminal session and its history |
| [pb session join](pb_session_join.md) | Join an explicitly shared terminal with recent output |
| [pb session list](pb_session_list.md) | List durable terminal sessions |
| [pb session participants](pb_session_participants.md) | Show terminal sharing and connected participants |
| [pb session remove](pb_session_remove.md) | Remove a teammate, including access through an all-team grant |
| [pb session rename](pb_session_rename.md) | Rename a terminal session |
| [pb session share](pb_session_share.md) | Grant a team or teammate viewer or interactive access |
| [pb session shared](pb_session_shared.md) | List owned and explicitly shared terminal sessions |
| [pb session unshare](pb_session_unshare.md) | End sharing while preserving the owner's terminal |
| [pb sessions](pb_sessions.md) |  |
| [pb sessions close](pb_sessions_close.md) | Close one or all terminal sessions |
| [pb sessions delete](pb_sessions_delete.md) | Delete a terminal session and its history |
| [pb sessions rename](pb_sessions_rename.md) | Rename a terminal session |
| [pb setup](pb_setup.md) | Set up this machine for Paperboat |
| [pb sftp](pb_sftp.md) | Run sftp with Paperboat machine resolution |
| [pb ssh](pb_ssh.md) | Connect to a machine with OpenSSH |
| [pb ssh doctor](pb_ssh_doctor.md) | Check SSH integration for a machine |
| [pb ssh trust-host](pb_ssh_trust-host.md) | Approve a changed SSH host identity |
| [pb status](pb_status.md) | Show local Paperboat machine status |
| [pb tag](pb_tag.md) | Assign tags to a device over gRPC IPC |
| [pb team](pb_team.md) | Manage teams and explicit resource permissions |
| [pb team accept](pb_team_accept.md) | Accept an invitation bound to this account |
| [pb team activity](pb_team_activity.md) | View owner/admin activity from the last 90 days |
| [pb team attach](pb_team_attach.md) | Attach or detach an explicitly selected personal resource |
| [pb team cancel-invite](pb_team_cancel-invite.md) | Cancel an outstanding invitation |
| [pb team create](pb_team_create.md) | Create a team |
| [pb team delete](pb_team_delete.md) | Delete team membership |
| [pb team get](pb_team_get.md) | Get teams |
| [pb team grant](pb_team_grant.md) | Set or revoke an explicit resource permission |
| [pb team invite](pb_team_invite.md) | Invite an account as a member |
| [pb team leave](pb_team_leave.md) | Leave team membership |
| [pb team list](pb_team_list.md) | List teams |
| [pb team machine](pb_team_machine.md) | Share machines and manage exact team access |
| [pb team machine grant](pb_team_machine_grant.md) | Set all-member or selected-member machine capabilities |
| [pb team machine remove](pb_team_machine_remove.md) | Revoke a team-owned machine without personal takeover |
| [pb team machine share](pb_team_machine_share.md) | Share your personal machine with a team; grants are separate |
| [pb team machine transfer-to-team](pb_team_machine_transfer-to-team.md) | Explicitly transfer your enrollment to team ownership |
| [pb team machine unshare](pb_team_machine_unshare.md) | Withdraw the team's grants while retaining personal ownership |
| [pb team remove](pb_team_remove.md) | Remove team membership |
| [pb team role](pb_team_role.md) | Role team membership |
| [pb team transfer](pb_team_transfer.md) | Transfer team membership |
| [pb transfer](pb_transfer.md) | Manage file transfers |
| [pb transfer cancel](pb_transfer_cancel.md) | Cancel a file transfer batch |
| [pb transfer destination](pb_transfer_destination.md) | Show the default transfer destination |
| [pb transfer destination clear](pb_transfer_destination_clear.md) | Clear the default transfer destination |
| [pb transfer destination set](pb_transfer_destination_set.md) | Set the default transfer destination |
| [pb transfer list](pb_transfer_list.md) | List file transfers |
| [pb transfer status](pb_transfer_status.md) | Inspect a file transfer |
| [pb tunnel](pb_tunnel.md) | Manage durable tunnels or start an ephemeral tunnel |
| [pb tunnel connector](pb_tunnel_connector.md) | Manage tunnel connectors |
| [pb tunnel connector add](pb_tunnel_connector_add.md) | Add a connector on this host |
| [pb tunnel connector drain](pb_tunnel_connector_drain.md) | Drain a tunnel connector |
| [pb tunnel connector list](pb_tunnel_connector_list.md) | List tunnel connectors |
| [pb tunnel connector revoke](pb_tunnel_connector_revoke.md) | Revoke a tunnel connector |
| [pb tunnel create](pb_tunnel_create.md) | Create a durable tunnel |
| [pb tunnel credentials](pb_tunnel_credentials.md) | Manage tunnel credentials |
| [pb tunnel credentials rotate](pb_tunnel_credentials_rotate.md) | Rotate tunnel connector credentials |
| [pb tunnel delete](pb_tunnel_delete.md) | Delete endpoints and routes and revoke connectors while preserving user DNS records |
| [pb tunnel doctor](pb_tunnel_doctor.md) | Diagnose tunnel health |
| [pb tunnel domain](pb_tunnel_domain.md) | Manage tunnel domains |
| [pb tunnel domain add](pb_tunnel_domain_add.md) | Add a tunnel domain |
| [pb tunnel domain instructions](pb_tunnel_domain_instructions.md) | Show authoritative DNS instructions for a tunnel domain |
| [pb tunnel domain list](pb_tunnel_domain_list.md) | List tunnel domains |
| [pb tunnel domain remove](pb_tunnel_domain_remove.md) | Remove a tunnel domain |
| [pb tunnel domain verify](pb_tunnel_domain_verify.md) | Verify a tunnel domain |
| [pb tunnel inspect](pb_tunnel_inspect.md) | Show daemon-local HTTP captures |
| [pb tunnel list](pb_tunnel_list.md) | List durable tunnels |
| [pb tunnel logs](pb_tunnel_logs.md) | Show tunnel logs |
| [pb tunnel pause](pb_tunnel_pause.md) | Pause new traffic while preserving tunnel identity and configuration |
| [pb tunnel policy](pb_tunnel_policy.md) | Manage permission to activate a private port on demand |
| [pb tunnel policy allow](pb_tunnel_policy_allow.md) | Allow on-demand access to an exact machine port |
| [pb tunnel policy get](pb_tunnel_policy_get.md) | Show an on-demand port policy |
| [pb tunnel policy revoke](pb_tunnel_policy_revoke.md) | Revoke an on-demand port policy |
| [pb tunnel replay](pb_tunnel_replay.md) | Deliberately replay one retained HTTP request to the same origin |
| [pb tunnel resume](pb_tunnel_resume.md) | Resume new traffic for a preserved tunnel |
| [pb tunnel route](pb_tunnel_route.md) | Manage tunnel routes |
| [pb tunnel route add](pb_tunnel_route_add.md) | Add a tunnel route |
| [pb tunnel route list](pb_tunnel_route_list.md) | List tunnel routes |
| [pb tunnel route remove](pb_tunnel_route_remove.md) | Remove a tunnel route |
| [pb tunnel route update](pb_tunnel_route_update.md) | Update a tunnel route |
| [pb tunnel show](pb_tunnel_show.md) | Show a durable tunnel |
| [pb tunnel status](pb_tunnel_status.md) | Show tunnel health |
| [pb tunnel stop](pb_tunnel_stop.md) | Stop an ephemeral tunnel |
| [pb uninstall](pb_uninstall.md) | Completely remove Paperboat from this machine |
| [pb update](pb_update.md) | Update pb from the signed Paperboat release |
| [pb update approve-maintenance](pb_update_approve-maintenance.md) | Approve one exact supervisor release for a protected-workload interruption |
| [pb update check](pb_update_check.md) | Check the signed Paperboat release without installing it |
| [pb update status](pb_update_status.md) | Show installed Paperboat update state |
| [pb wait](pb_wait.md) | Wait for a machine readiness condition |
