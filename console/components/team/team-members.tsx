"use client"

import { CaretDownIcon, CrownSimpleIcon, SignOutIcon, UserMinusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useSearchParams } from "next/navigation"
import { useState } from "react"
import { toast } from "sonner"
import { meQuery, useApiQuery, useWorkspace, workspacesQuery, type Me, type Workspace } from "@/lib/api"
import { billingKey, billingQuery, notLiveTitle } from "@/lib/billing"
import {
  answerMachine,
  assignableRoles,
  cancelInvitation,
  resendInvitation,
  memberEmails,
  roleDescriptions,
  roleLabels,
  roleOrder,
  teamAction,
  teamInvitationsQuery,
  teamLive,
  teamQuery,
  type Team,
  type TeamInvitation,
  type TeamMember,
  type TeamRole,
} from "@/lib/teams"
import { setWorkspace } from "@/lib/workspace"
import { cn } from "@/lib/utils"
import { CopyButton } from "@/components/copy-button"
import { InviteDialog } from "@/components/team/invite-dialog"
import { TeamSecurity } from "@/components/team/team-security"
import { RecordingSettings } from "@/components/recordings/recording-settings"
import { TeamStorage } from "@/components/team/team-storage"
import { RowMenus, type Entry } from "@/components/row-menus"
import { ConfirmAction, SearchField, ViewToggle } from "@/components/list-toolbar"
import { Time } from "@/components/time"
import { MachineAccess } from "@/components/team/machine-access"
import { DangerZone as DangerZonePanel, Section, SettingRow } from "@/components/section"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ListSkeleton } from "@/components/skeletons"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

/**
 * Who can do what, from the server's rules (teams/service.go Mutate, teams-business.md). A member
 * managed by the company directory changes role or leaves only through the directory.
 */
function permissions(actor: TeamRole, target: TeamMember, you: boolean) {
  const owner = actor === "owner"
  const managed = Boolean(target.directory_managed)
  const roles = you || managed ? [] : assignableRoles(actor, target.role)
  return {
    roles,
    transfer: owner && !you && !managed,
    remove: !you && !managed && target.role !== "owner" && (owner || (actor === "admin" && target.role !== "admin")),
    leave: you && actor !== "owner" && !managed,
  }
}

function useRefreshTeam(teamId: string) {
  const queryClient = useQueryClient()
  return () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: teamQuery(teamId).queryKey }),
      queryClient.invalidateQueries({ queryKey: teamInvitationsQuery(teamId).queryKey }),
      // Pending invitations hold seats, so seat counts change with them.
      queryClient.invalidateQueries({ queryKey: billingKey(teamId) }),
    ])
}

export function TeamManage({ workspace }: { workspace: Workspace }) {
  const team = useApiQuery(teamQuery(workspace.id))
  const invitations = useApiQuery(teamInvitationsQuery(workspace.id))
  const billing = useApiQuery(billingQuery(workspace))
  const me = useApiQuery(meQuery)
  const [view, setView] = useState<"members" | "invitations">("members")
  const [search, setSearch] = useState("")
  const [inviting, setInviting] = useState(false)
  const tabParam = useSearchParams().get("tab")
  const tab = tabParam === "security" || tabParam === "recordings" || tabParam === "storage" ? tabParam : "members"

  if (team.isPending || me.isPending) return <TeamSkeleton />
  if (team.isError) return <p className="text-sm text-muted-foreground">{team.error.message}</p>
  if (!me.data) return null

  const emails = memberEmails(team.data, invitations.data)
  emails.set(me.data.id, me.data.email)
  const actor = (team.data.members.find((m) => m.account_id === me.data!.id)?.role ?? "member") as TeamRole
  // Members and settings are managed by the owner and admins; billing admins and auditors only see them.
  const admin = actor === "owner" || actor === "admin"
  const query = search.trim().toLowerCase()
  const members = team.data.members
    .filter((m) => m.active)
    .sort(
      (a, b) =>
        roleOrder[a.role] - roleOrder[b.role] ||
        (emails.get(a.account_id) ?? "").localeCompare(emails.get(b.account_id) ?? ""),
    )
  const pending = (invitations.data ?? []).filter((i) => i.state === "pending")
  const matches = (...texts: (string | undefined)[]) => !query || texts.some((t) => t?.toLowerCase().includes(query))
  const shownMembers = members.filter((m) => matches(emails.get(m.account_id), m.display_name))
  const shownInvites = pending.filter((i) => matches(i.email))
  const seats = billing.data && billing.data.seats_bought
  // The server counts seats (members plus invitations that hold one).
  const usedSeats = billing.data?.seats_used ?? members.length
  const freeSeats = billing.data ? Math.max(0, billing.data.seats_bought - billing.data.seats_used) : undefined
  const name = team.data.display_name || team.data.team_id
  // Security (SSO, directory) is for the owner and admins to change and auditors to see.
  const securityTab = teamLive.security && ["owner", "admin", "auditor"].includes(actor)
  // Recording settings: owners and admins change them, auditors see them.
  const recordingsTab = ["owner", "admin", "auditor"].includes(actor)
  // The team's bucket: owners and admins connect and change it.
  const storageTab = admin
  const tabs = (["members", "security", "recordings", "storage"] as const).filter(
    (value) => value === "members" || (value === "security" ? securityTab : value === "recordings" ? recordingsTab : storageTab),
  )
  const tabTitles = { members: "Members", security: "Security", recordings: "Recordings", storage: "Storage" }

  return (
    <div className="flex flex-col gap-10">
      <header className="flex flex-wrap items-center gap-4">
        <Avatar className="size-12 rounded-lg">
          <AvatarFallback className="rounded-lg bg-primary/12 text-lg font-semibold text-primary">
            {name.slice(0, 1).toUpperCase()}
          </AvatarFallback>
        </Avatar>
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <h1 className="truncate font-heading text-2xl font-semibold tracking-tight">{name}</h1>
          <p className="flex flex-wrap items-center gap-x-2 text-sm text-muted-foreground">
            <span>
              {members.length} {members.length === 1 ? "member" : "members"}
            </span>
            {seats ? (
              <>
                <span aria-hidden>·</span>
                <Link href="/billing" className="hover:text-foreground hover:underline">
                  {usedSeats} of {seats} seats used
                </Link>
              </>
            ) : null}
            <span aria-hidden>·</span>
            <span>Your role: {roleLabels[actor]}</span>
          </p>
        </div>
        {admin && (
          <Button size="lg" disabled={team.data.suspended} onClick={() => setInviting(true)}>
            Invite members
          </Button>
        )}
      </header>

      {team.data.suspended && <SuspendedNotice owner={actor === "owner"} />}

      {tabs.length > 1 && (
        <nav className="-mb-4 flex gap-1 border-b border-divider" aria-label="Team">
          {tabs.map((value) => (
            <Link
              key={value}
              href={value === "members" ? "/team" : `/team?team=${encodeURIComponent(team.data.team_id)}&tab=${value}`}
              scroll={false}
              className={cn(
                "-mb-px border-b-2 border-transparent px-2.5 pb-2.5 text-sm text-muted-foreground hover:text-foreground",
                tab === value && "border-foreground text-foreground",
              )}
            >
              {tabTitles[value]}
            </Link>
          ))}
        </nav>
      )}
      {securityTab && tab === "security" ? (
        <TeamSecurity team={team.data} emails={emails} />
      ) : recordingsTab && tab === "recordings" ? (
        <RecordingSettings team={team.data} emails={emails} admin={admin} />
      ) : storageTab && tab === "storage" ? (
        <TeamStorage team={team.data.team_id} admin={admin} />
      ) : (
        <>
        <MachineRequests team={team.data} admin={admin} me={me.data} />
        <MachineAccess team={team.data} admin={admin} accountId={me.data.id} emails={emails} />

        <section className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <ViewToggle
              value={view}
              onChange={setView}
              views={[
                { value: "members", label: "Members", count: members.length },
                ...(admin ? [{ value: "invitations" as const, label: "Invitations", count: pending.length }] : []),
              ]}
            />
            <div className="ml-auto w-full max-w-sm">
              <SearchField value={search} onChange={setSearch} placeholder="Search by name or email" />
            </div>
          </div>
          {view === "members" ? (
            <MembersTable team={team.data} members={shownMembers} emails={emails} me={me.data} actor={actor} />
          ) : invitations.isError ? (
            <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
              Couldn’t load invitations.
              <Button variant="outline" size="sm" onClick={() => invitations.refetch()}>
                Retry
              </Button>
            </div>
          ) : (
            <InvitationsTable team={team.data} invitations={shownInvites} emails={emails} me={me.data} />
          )}
        </section>

        {admin && <TeamSettings team={team.data} owner={actor === "owner"} />}
        <DangerZone team={team.data} owner={actor === "owner"} />
        </>
      )}
      <InviteDialog
        team={team.data}
        owner={actor === "owner"}
        freeSeats={freeSeats}
        open={inviting}
        onOpenChange={setInviting}
      />
    </div>
  )
}

/** A suspended team keeps its members but nothing else works (409 team_suspended on changes). */
function SuspendedNotice({ owner }: { owner: boolean }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-4 rounded-lg bg-destructive/5 px-4 py-3 text-sm dark:bg-destructive/10">
      <span className="text-destructive">
        This team is suspended: members, machines and sharing are paused until the owner restores it.
      </span>
      {owner ? (
        <Button nativeButton={false} render={<Link href="/billing" />}>
          Restore billing
        </Button>
      ) : (
        <span className="text-muted-foreground">Ask the team owner to restore the team.</span>
      )}
    </div>
  )
}

/**
 * The team header (avatar, name and detail line at their sizes; Invite members, which owners and
 * admins have) over the members toolbar as it is and member rows as placeholders.
 */
function TeamSkeleton() {
  return (
    <div className="flex flex-col gap-10">
      <header className="flex flex-wrap items-center gap-4">
        <Skeleton className="size-12 rounded-lg" />
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <Skeleton className="my-1 h-6 w-48" />
          <Skeleton className="my-0.5 h-4 w-64" />
        </div>
        <Button size="lg" tabIndex={-1} aria-hidden className="pointer-events-none">
          Invite members
        </Button>
      </header>
      <section className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <ViewToggle
            value="members"
            onChange={() => {}}
            views={[
              { value: "members", label: "Members" },
              { value: "invitations", label: "Invitations" },
            ]}
          />
          <div className="ml-auto w-full max-w-sm">
            <SearchField value="" onChange={() => {}} placeholder="Search by name or email" />
          </div>
        </div>
        <ListSkeleton rows={4} />
      </section>
    </div>
  )
}

function Person({ email, name, you }: { email?: string; name?: string; you?: boolean }) {
  const label = name || email || "Pending sign-in"
  return (
    <div className="flex min-w-0 items-center gap-3">
      <Avatar className="size-8">
        <AvatarFallback className="bg-muted text-xs font-medium">
          {(name || email || "?").slice(0, 2).toUpperCase()}
        </AvatarFallback>
      </Avatar>
      <div className="flex min-w-0 flex-col">
        <span className="flex items-center gap-2 font-medium">
          <span className="truncate">{label}</span>
          {you && (
            <Badge variant="secondary" className="trim-cap">
              You
            </Badge>
          )}
        </span>
        {name && email && <span className="truncate text-xs text-muted-foreground">{email}</span>}
      </div>
    </div>
  )
}

function MembersTable({
  team,
  members,
  emails,
  me,
  actor,
}: {
  team: Team
  members: TeamMember[]
  emails: Map<string, string>
  me: Me
  actor: TeamRole
}) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        <col />
        <col className="w-36" />
        <col className="hidden w-36 md:table-column" />
        <col className="w-12" />
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          <th>Member</th>
          <th>Role</th>
          <th className="hidden md:table-cell">Joined</th>
          <th aria-label="Actions" />
        </tr>
      </thead>
      <tbody className="divide-y divide-divider">
        {members.map((member) => (
          <MemberRow
            key={member.account_id}
            team={team}
            member={member}
            email={emails.get(member.account_id)}
            you={member.account_id === me.id}
            actor={actor}
          />
        ))}
      </tbody>
      {members.length === 0 && (
        <caption className="caption-bottom py-12 text-sm text-muted-foreground">No members match</caption>
      )}
    </table>
  )
}

function MemberRow({
  team,
  member,
  email,
  you,
  actor,
}: {
  team: Team
  member: TeamMember
  email?: string
  you: boolean
  actor: TeamRole
}) {
  const [confirm, setConfirm] = useState<"remove" | "transfer" | "leave" | null>(null)
  const refresh = useRefreshTeam(team.team_id)
  const can = permissions(actor, member, you)
  const role = useMutation({
    mutationFn: (next: string) => teamAction(team, "role", { account_id: member.account_id, role: next }),
    onSuccess: refresh,
  })
  const who = member.display_name || email || "this member"
  // The row's "…" and right-click menu (RowMenus), as on every table.
  const entries: Entry[] = [
    ...(can.transfer
      ? [
          {
            kind: "item" as const,
            label: "Transfer ownership",
            icon: CrownSimpleIcon,
            onSelect: () => setConfirm("transfer"),
          },
        ]
      : []),
    ...(can.remove
      ? [
          {
            kind: "item" as const,
            label: "Remove from team",
            icon: UserMinusIcon,
            destructive: true,
            onSelect: () => setConfirm("remove"),
          },
        ]
      : []),
    ...(can.leave
      ? [
          {
            kind: "item" as const,
            label: "Leave team",
            icon: SignOutIcon,
            destructive: true,
            onSelect: () => setConfirm("leave"),
          },
        ]
      : []),
  ]
  const dialogs = (
    <>
      <ConfirmAction
        open={confirm === "remove"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title={`Remove ${who}?`}
        description={`${who} loses access to the team's machines, ENV and config now. The seat stays paid until the billing date and can be reused.`}
        action="Remove"
        pending="Removing…"
        mutation={{
          mutationFn: () => teamAction(team, "remove", { account_id: member.account_id }),
          onSuccess: refresh,
        }}
      />
      <ConfirmAction
        open={confirm === "transfer"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title={`Make ${who} the owner?`}
        description={`${who} becomes owner with billing and delete rights. You become a member; only ${who} can give ownership back.`}
        action="Transfer ownership"
        pending="Transferring…"
        mutation={{
          mutationFn: () => teamAction(team, "transfer", { account_id: member.account_id }),
          onSuccess: refresh,
        }}
      />
      {confirm === "leave" && <LeaveDialog team={team} onClose={() => setConfirm(null)} />}
    </>
  )
  const row = (actions: React.ReactNode) => (
    <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
      <td>
        <Person email={email} name={member.display_name} you={you} />
      </td>
      <td>
        {can.roles.length ? (
          <DropdownMenu>
            <DropdownMenuTrigger className="flex items-center gap-1 rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30">
              {roleLabels[member.role]}
              <CaretDownIcon className="size-3" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="min-w-64">
              <DropdownMenuRadioGroup value={member.role} onValueChange={(next) => role.mutate(next as string)}>
                {can.roles.map((value) => (
                  <DropdownMenuRadioItem key={value} value={value}>
                    <span className="flex flex-col">
                      {roleLabels[value]}
                      <span className="text-xs text-muted-foreground">{roleDescriptions[value]}</span>
                    </span>
                  </DropdownMenuRadioItem>
                ))}
              </DropdownMenuRadioGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        ) : (
          <span className="text-muted-foreground">{roleLabels[member.role] ?? member.role}</span>
        )}
        {member.directory_managed && (
          <Badge variant="outline" className="mt-1 block w-fit font-normal text-muted-foreground">
            Managed by directory
          </Badge>
        )}
        {role.isError && <span className="block text-xs text-destructive">{role.error.message}</span>}
      </td>
      <td className="hidden text-muted-foreground md:table-cell">
        {member.joined_at ? <Time iso={member.joined_at} relative /> : "—"}
      </td>
      <td className="text-right">{actions}</td>
    </tr>
  )
  if (!entries.length) return row(null)
  return <RowMenus label={`Actions for ${who}`} entries={entries} dialogs={dialogs} row={row} />
}

function InvitationsTable({
  team,
  invitations,
  emails,
  me,
}: {
  team: Team
  invitations: TeamInvitation[]
  emails: Map<string, string>
  me: Me
}) {
  const refresh = useRefreshTeam(team.team_id)
  const revoke = useMutation({
    mutationFn: (id: string) => cancelInvitation(team, id),
    onSuccess: refresh,
    onError: (error) => toast.error("The invitation wasn’t revoked", { description: error.message }),
  })
  const resend = useMutation({
    mutationFn: (id: string) => resendInvitation(team, id),
    onSuccess: refresh,
    onError: (error) => toast.error("The invitation wasn’t sent again", { description: error.message }),
  })
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        <col />
        <col className="w-28" />
        <col className="hidden w-28 md:table-column" />
        <col className="hidden w-28 md:table-column" />
        <col className="w-40" />
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          <th>Email</th>
          <th>Role</th>
          <th className="hidden md:table-cell">Invited by</th>
          <th className="hidden md:table-cell">Expires</th>
          <th aria-label="Actions" />
        </tr>
      </thead>
      <tbody className="divide-y divide-divider">
        {invitations.map((invitation) => {
          const by =
            invitation.created_by === me.id
              ? "You"
              : (invitation.created_by_email ?? emails.get(invitation.created_by) ?? "—")
          return (
            <tr key={invitation.invitation_id} className="hover:bg-surface [&>td]:h-14 [&>td]:px-3">
              <td>
                <Person email={invitation.email} />
                {teamLive.mail && <Delivery invitation={invitation} />}
              </td>
              <td className="text-muted-foreground">{roleLabels[invitation.role] ?? invitation.role}</td>
              <td className="hidden truncate text-muted-foreground md:table-cell">{by}</td>
              <td className="hidden text-muted-foreground md:table-cell">
                <Time iso={invitation.expires_at} relative />
              </td>
              <td className="text-right">
                <Button
                  variant="ghost"
                  disabled={!teamLive.mail || resend.isPending}
                  title={teamLive.mail ? "Send the email again; the invitation gets 30 more days" : notLiveTitle}
                  onClick={() => resend.mutate(invitation.invitation_id)}
                >
                  Resend
                </Button>
                <Button
                  variant="ghost"
                  disabled={revoke.isPending}
                  onClick={() => revoke.mutate(invitation.invitation_id)}
                >
                  Revoke
                </Button>
              </td>
            </tr>
          )
        })}
      </tbody>
      {invitations.length === 0 && (
        <caption className="caption-bottom py-12 text-sm text-muted-foreground">No pending invitations</caption>
      )}
      {(revoke.error ?? resend.error) && (
        <caption className="caption-bottom py-2 text-sm text-destructive">
          {(revoke.error ?? resend.error)!.message}
        </caption>
      )}
    </table>
  )
}

const deliveryLabels = {
  queued: "Sending email…",
  sent: "Email sent",
  failed: "Email failed, resend it",
  unconfigured: "Email not set up, share the link",
} as const

/** Whether the invitation email went out, under the address (Resend via the server's outbox). */
function Delivery({ invitation }: { invitation: TeamInvitation }) {
  if (!invitation.delivery_state) return null
  return (
    <span
      className={cn(
        "block pl-11 text-xs",
        invitation.delivery_state === "failed" ? "text-destructive" : "text-muted-foreground",
      )}
    >
      {deliveryLabels[invitation.delivery_state]}
      {invitation.delivery_state === "sent" && invitation.last_sent_at && (
        <>
          {" "}
          <Time iso={invitation.last_sent_at} relative />
        </>
      )}
    </span>
  )
}

/**
 * Machines that asked to join the team: owners and admins accept or decline them; a member sees
 * where their own stand and can withdraw them.
 */
function MachineRequests({ team, admin, me }: { team: Team; admin: boolean; me: Me | undefined }) {
  const refresh = useRefreshTeam(team.team_id)
  const queryClient = useQueryClient()
  const answer = useMutation({
    mutationFn: ({ machineId, action }: { machineId: string; action: "approve" | "decline" | "unshare" }) =>
      answerMachine(team, machineId, action),
    onSuccess: async () => {
      await refresh()
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
    },
  })
  const requests = team.machine_requests ?? []
  if (!requests.length) return null
  return (
    <Section title="Machines waiting">
      <ul className="flex flex-col">
        {requests.map((request) => {
          const mine = request.requested_by === me?.id
          return (
            <li key={request.machine_id} className="flex min-h-12 flex-wrap items-center gap-3 py-2 text-sm">
              <span className="min-w-0 flex-1">
                <span className="font-medium">{request.alias}</span>
                <span className="text-muted-foreground">
                  {request.state === "declined"
                    ? mine
                      ? " was declined by an owner or admin"
                      : " was declined"
                    : mine
                      ? " is waiting for an owner or admin"
                      : " asked to join"}
                </span>
              </span>
              {admin && request.state === "pending" && (
                <>
                  <Button
                    variant="outline"
                    disabled={answer.isPending}
                    onClick={() => answer.mutate({ machineId: request.machine_id, action: "decline" })}
                  >
                    Decline
                  </Button>
                  <Button
                    disabled={answer.isPending}
                    onClick={() => answer.mutate({ machineId: request.machine_id, action: "approve" })}
                  >
                    Accept
                  </Button>
                </>
              )}
              {mine && (
                <Button
                  variant="outline"
                  disabled={answer.isPending}
                  onClick={() => answer.mutate({ machineId: request.machine_id, action: "unshare" })}
                >
                  Withdraw
                </Button>
              )}
            </li>
          )
        })}
      </ul>
      {answer.isError && <p className="py-2 text-sm text-destructive">{answer.error.message}</p>}
    </Section>
  )
}

function TeamSettings({ team, owner }: { team: Team; owner: boolean }) {
  const refresh = useRefreshTeam(team.team_id)
  const queryClient = useQueryClient()
  const [name, setName] = useState(team.display_name || team.team_id)
  const rename = useMutation({
    mutationFn: () => teamAction(team, "rename", { name: name.trim() }),
    onSuccess: async () => {
      await refresh()
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
    },
  })
  const changed = name.trim() !== (team.display_name || team.team_id)
  const privateAccess = useMutation({
    mutationFn: (on: boolean) => teamAction(team, "private_access", { allow_private_access: on }),
    onSuccess: async () => {
      await refresh()
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
    },
  })
  const approval = useMutation({
    mutationFn: (on: boolean) => teamAction(team, "machine_approval", { approve_new_machines: on }),
    onSuccess: async () => {
      await refresh()
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
    },
  })
  return (
    <Section title="Settings">
      <SettingRow
        label="Approve new machines"
        htmlFor="team-approval"
        description="Machines added by members wait for an owner or admin to accept them."
      >
        <Switch
          id="team-approval"
          checked={team.approve_new_machines ?? true}
          disabled={approval.isPending}
          onCheckedChange={(on) => approval.mutate(on)}
          aria-label="Approve new machines"
        />
      </SettingRow>
      {approval.isError && <p className="py-2 text-sm text-destructive">{approval.error.message}</p>}
      <SettingRow
        label="Allow private access"
        htmlFor="team-private-access"
        description="Members can reach ports on the team’s shared machines over the private network."
      >
        <Switch
          id="team-private-access"
          checked={team.allow_private_access ?? false}
          disabled={privateAccess.isPending}
          onCheckedChange={(on) => privateAccess.mutate(on)}
          aria-label="Allow private access"
        />
      </SettingRow>
      {privateAccess.isError && <p className="py-2 text-sm text-destructive">{privateAccess.error.message}</p>}
      {owner && (
        <SettingRow label="Team name" htmlFor="team-name">
          <form
            className="flex items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              if (changed && name.trim()) rename.mutate()
            }}
            title={teamLive.rename ? undefined : notLiveTitle}
          >
            <Input
              id="team-name"
              className="w-56"
              value={name}
              disabled={!teamLive.rename}
              onChange={(event) => setName(event.target.value)}
            />
            <Button type="submit" variant="outline" disabled={!teamLive.rename || !changed || rename.isPending}>
              Save
            </Button>
          </form>
        </SettingRow>
      )}
      <SettingRow label="Team ID">
        <span className="flex items-center gap-1 font-mono text-code">
          {team.team_id}
          <CopyButton value={team.team_id} label="Copy team ID" />
        </span>
      </SettingRow>
      {rename.isError && <p className="py-2 text-sm text-destructive">{rename.error.message}</p>}
    </Section>
  )
}

function LeaveDialog({ team, onClose }: { team: Team; onClose: () => void }) {
  const queryClient = useQueryClient()
  return (
    <ConfirmAction
      open
      onOpenChange={(open) => !open && onClose()}
      title={`Leave ${team.display_name || team.team_id}?`}
      description="You lose access to the team’s machines, ENV and config. You’ll need a new invitation to return."
      action="Leave team"
      pending="Leaving…"
      mutation={{
        mutationFn: () => teamAction(team, "leave"),
        onSuccess: async () => {
          setWorkspace("personal")
          await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
        },
      }}
    />
  )
}

/** Owners delete (billing must be cancelled first); everyone else leaves. DESIGN.md: Danger zone. */
function DangerZone({ team, owner }: { team: Team; owner: boolean }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const [open, setOpen] = useState(false)
  return (
    <DangerZonePanel
      title={owner ? "Delete team" : "Leave team"}
      description={
        owner
          ? "Members lose access, team ENV and config are deleted, and machines return to their owners. Cancel the plan in Billing first."
          : "You lose access to the team's machines, ENV and config."
      }
      action={owner ? "Delete team" : "Leave team"}
      onAction={() => setOpen(true)}
    >
      {open && !owner && <LeaveDialog team={team} onClose={() => setOpen(false)} />}
      {owner && (
        <ConfirmAction
          open={open}
          onOpenChange={setOpen}
          title={`Delete ${team.display_name || team.team_id}?`}
          description="This can't be undone."
          action="Delete team"
          pending="Deleting…"
          confirmName={team.team_id}
          mutation={{
            mutationFn: () => teamAction(team, "delete", { confirmation: team.team_id }),
            onSuccess: async () => {
              if (workspace?.id === team.team_id) setWorkspace("personal")
              await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
            },
          }}
        />
      )}
    </DangerZonePanel>
  )
}
