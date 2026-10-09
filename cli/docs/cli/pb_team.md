## pb team

Manage teams and explicit resource permissions

### Synopsis

Manage teams and explicit resource permissions. Owners appoint admins, transfer ownership, delete teams and reset ENV. Admins manage ordinary members and grants; ENV rotation requires authorized keys. Owners must transfer ownership before leaving. Removal ends team access and adopted defaults; independently granted Git access and previously received files or secrets remain. Team deletion revokes team-owned machines and preserves personal resources.

With --json, this command group lists its available commands.

```
pb team [flags]
```

### Options

```
  -h, --help   help for team
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb team accept](pb_team_accept.md)	 - Accept an invitation bound to this account
* [pb team activity](pb_team_activity.md)	 - View owner/admin activity from the last 90 days
* [pb team attach](pb_team_attach.md)	 - Attach or detach an explicitly selected personal resource
* [pb team cancel-invite](pb_team_cancel-invite.md)	 - Cancel an outstanding invitation
* [pb team create](pb_team_create.md)	 - Create a team
* [pb team decline](pb_team_decline.md)	 - Decline an invitation bound to this account
* [pb team delete](pb_team_delete.md)	 - Delete a team
* [pb team get](pb_team_get.md)	 - Show one team and its current generation
* [pb team grant](pb_team_grant.md)	 - Set or revoke an explicit resource permission
* [pb team invitation](pb_team_invitation.md)	 - Show an invitation you may receive or administer
* [pb team invitations](pb_team_invitations.md)	 - Discover pending invitations received by you or administered for a team
* [pb team invite](pb_team_invite.md)	 - Invite an account as a member
* [pb team leave](pb_team_leave.md)	 - Leave a team
* [pb team list](pb_team_list.md)	 - List teams
* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access
* [pb team remove](pb_team_remove.md)	 - Remove a team member
* [pb team role](pb_team_role.md)	 - Change a team member's role
* [pb team transfer](pb_team_transfer.md)	 - Transfer team ownership

