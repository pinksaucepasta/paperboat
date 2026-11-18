package main

const teamGenerationNote = `Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.`

var teamHelpTexts = map[string]helpText{
	"pb team": {Example: `
  pb team list
  pb team get acme
  pb team invite acme ana@example.test --generation 7`},
	"pb team list": {
		Long: `List the teams this account belongs to, one line each: team ID, owner account and current
generation, sorted by team ID. Use pb team get for members, grants and machines.`,
		Example: `
  pb team list
  pb team list --json`},
	"pb team get": {
		Long: `Show one team: its owner, current generation, state (active, suspended or deleted), the
members and resource grants, the machines shared with or owned by the team, and the team's ENV
status. Every change to the team needs the generation printed here.`,
		Example: `
  pb team get acme
  pb team get acme --json`},
	"pb team create": {
		Long: `Create a team by paying for its seats. The command prints a Checkout link, opens it in a
browser when it can, and then waits until payment completes and the team exists. The owner uses
one of the seats.

Paying needs your agreement to the no-refund terms: without --accept-terms the command prints
them and stops. Add --accept-terms to agree and continue.

Use --no-wait to print the link and return. Ctrl+C or --wait-timeout stops only the waiting; the
team is still created once you pay, and pb team get shows it. Running the same command again
reuses the same order instead of billing twice.`,
		Example: `
  pb team create acme --seats 5 --accept-terms
  pb team create acme --seats 5 --interval year --no-wait --accept-terms
  pb team create acme --json`},
	"pb team invite": {
		Long: `Invite a person by email address. They do not need an account yet: they sign in, verify
that email address, then accept the invitation. The invitation is for the member role and
expires; change the role afterwards with pb team role.

The team needs a spare seat, otherwise the invitation is refused and you must add seats first.
` + teamGenerationNote,
		Example: `
  pb team get acme
  pb team invite acme ana@example.test --generation 7`},
	"pb team accept": {
		Long: `Accept an invitation addressed to this account and join the team. Find the invitation ID
with pb team invitations. The command prints the team as it is after you joined.`,
		Example: `
  pb team invitations
  pb team accept inv_abc123`},
	"pb team decline": {
		Long: `Decline an invitation addressed to this account, so you do not join the team. The invitation
ID comes from pb team invitations. Invitations addressed to other accounts cannot be declined.`,
		Example: `
  pb team invitations
  pb team decline inv_abc123`},
	"pb team cancel-invite": {
		Long: `Cancel an invitation that has not been accepted yet, so it can no longer be used. Team
administrators can cancel invitations of their team; find the ID with pb team invitations <team>.
` + teamGenerationNote,
		Example: `
  pb team invitations acme
  pb team cancel-invite acme inv_abc123 --generation 8`},
	"pb team role": {
		Long: `Change one member's role: admin, billing, auditor or member. Admins may only change
billing admins, auditors and members; appointing admins is an owner's job. The role changes who
manages the team, not the resource grants the member already has.
` + teamGenerationNote,
		Example: `
  pb team role acme ana auditor --generation 8`},
	"pb team remove": {
		Long: `Remove a member from the team. Their team access and adopted defaults end at once.
Independently granted Git access and files or secrets they already received remain with them.
` + teamGenerationNote,
		Example: `
  pb team remove acme ana --generation 9`},
	"pb team leave": {
		Long: `Leave a team yourself. An owner must transfer ownership first (pb team transfer) before
leaving.
` + teamGenerationNote,
		Example: `
  pb team leave acme --generation 9`},
	"pb team transfer": {
		Long: `Transfer team ownership to another member's account. Only an owner can transfer ownership.
` + teamGenerationNote,
		Example: `
  pb team transfer acme ana --generation 9`},
	"pb team delete": {
		Long: `Delete a team. Team-owned machines are revoked; personal resources stay with their owners.
Check pb team get first for machines and attached resources.

--confirm must repeat the exact team ID, so a typo cannot delete the wrong team.
` + teamGenerationNote,
		Example: `
  pb team delete acme --confirm acme --generation 9`},
	"pb team grant": {
		Long: `Give one member a permission on one resource, or revoke it with --active=false. The
permission is read or write for an ENV vault (env), and use, manage, inspect or replay for a
preview or tunnel. Each permission stands alone: manage does not imply inspect, and inspect does
not imply replay. Team membership alone grants none of them.
` + teamGenerationNote,
		Example: `
  pb team grant acme ana env vault1 read --generation 9
  pb team grant acme ana preview blog manage --generation 9
  pb team grant acme ana preview blog manage --active=false --generation 10`},
	"pb team attach": {
		Long: `Attach one of your personal previews or tunnels to the team so team grants can refer to it,
or detach it with --active=false. Only previews and tunnels can be attached.
` + teamGenerationNote,
		Example: `
  pb team attach acme preview blog --generation 9
  pb team attach acme preview blog --active=false --generation 10`},
	"pb team invitations": {
		Long: `List pending invitations: the ones addressed to you, or with a team argument the ones
that team has outstanding (owners and admins). Each line shows invitation ID, team, recipient,
state and expiry. Filter with --q (email or ID); the list stops at --limit unless you pass --all.`,
		Example: `
  pb team invitations
  pb team invitations acme --q ana
  pb team invitations --all --json`},
	"pb team invitation": {
		Long: `Show one invitation you received or administer, as JSON. The invitation ID comes from pb team
invitations. This only displays it; use accept, decline or cancel-invite to act on it.`,
		Example: `
  pb team invitation inv_abc123`},
	"pb team activity": {Example: `
  pb team activity acme --limit 20
  pb team activity acme --action billing --since 7d
  pb team activity acme --result denied --json`},
	"pb team machine": {Example: `
  pb team machine share acme m_abc123 --generation 9
  pb team machine grant acme m_abc123 --all-members --capability terminal --generation 10
  pb team machine requests acme`},
	"pb team machine share": {
		Long: `Share your personal machine with the team. You keep ownership; sharing alone lets nobody
use the machine, so add a grant with pb team machine grant. If the team requires approval for
new machines (pb team machine approval), the machine waits until an owner or admin accepts it.
` + teamGenerationNote,
		Example: `
  pb team machine share acme m_abc123 --generation 9`},
	"pb team machine unshare": {
		Long: `Withdraw the team's access to a machine of yours. Its grants end and you stay the owner.
` + teamGenerationNote,
		Example: `
  pb team machine unshare acme m_abc123 --generation 9`},
	"pb team machine transfer-to-team": {
		Long: `Give ownership of a machine you enrolled to the team. This changes who owns and can
revoke the machine, so --confirm must repeat the exact machine ID.
` + teamGenerationNote,
		Example: `
  pb team machine transfer-to-team acme m_abc123 --confirm m_abc123 --generation 9`},
	"pb team machine transfer-to-personal": {
		Long: `Return a team-owned machine to the personal account that enrolled it. --confirm must
repeat the exact machine ID.
` + teamGenerationNote,
		Example: `
  pb team machine transfer-to-personal acme m_abc123 --confirm m_abc123 --generation 9`},
	"pb team machine remove": {
		Long: `Revoke a team-owned machine's enrollment without handing it to a person. --confirm must
repeat the exact machine ID. If the device was lost or compromised, rotate the team's ENV keys
afterwards.
` + teamGenerationNote,
		Example: `
  pb team machine remove acme m_abc123 --confirm m_abc123 --generation 9`},
	"pb team machine grant": {Example: `
  pb team machine grant acme m_abc123 --all-members --capability terminal --capability exec --generation 9
  pb team machine grant acme m_abc123 --member ana --capability terminal --generation 10
  pb team machine grant acme m_abc123 --all-members --capability terminal --active=false --generation 11`},
	"pb team machine approve": {
		Long: `Accept a machine a member asked to add to the team (see pb team machine requests). Only
owners and admins can. --generation is optional here: without it the command uses the team's
current generation.`,
		Example: `
  pb team machine requests acme
  pb team machine approve acme m_abc123`},
	"pb team machine decline": {
		Long: `Decline a machine a member asked to add to the team. Only owners and admins can.
--generation is optional: without it the command uses the team's current generation.`,
		Example: `
  pb team machine decline acme m_abc123`},
	"pb team machine requests": {
		Long: `List machines waiting for an owner or admin to accept them, one line each with machine ID,
name, state and who asked. Prints "No machines are waiting." when there are none.`,
		Example: `
  pb team machine requests acme
  pb team machine requests acme --json`},
	"pb team machine approval": {
		Long: `Choose whether machines members add to the team wait for an owner or admin. With on, a new
machine waits until pb team machine approve; with off, it joins right away.`,
		Example: `
  pb team machine approval acme on
  pb team machine approval acme off`},
}
