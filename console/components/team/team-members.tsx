"use client"

import { CaretDownIcon, DotsThreeIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { meQuery, useApiQuery, useWorkspace, workspacesQuery, type Me, type Workspace } from "@/lib/api"
import { billingQuery, notLiveTitle } from "@/lib/billing"
import {
  cancelInvitation,
  memberEmails,
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
import { ConfirmAction, SearchField, ViewToggle } from "@/components/list-toolbar"
import { Time } from "@/components/time"
import { Row, Section } from "@/components/section"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

const roleLabels: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member" }
const roleOrder: Record<string, number> = { owner: 0, admin: 1, member: 2 }

/** Who can do what, from the server's rules (teams/service.go Mutate). */
function permissions(actor: TeamRole, target: TeamMember, you: boolean) {
  const owner = actor === "owner"
  return {
    changeRole: owner && !you && target.role !== "owner",
    transfer: owner && !you,
    remove: !you && target.role !== "owner" && (owner || (actor === "admin" && target.role === "member")),
    leave: you && actor !== "owner",
  }
}

function useRefreshTeam(teamId: string) {
  const queryClient = useQueryClient()
  return () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: teamQuery(teamId).queryKey }),
      queryClient.invalidateQueries({ queryKey: teamInvitationsQuery(teamId).queryKey }),
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

  if (team.isPending || me.isPending) return <TeamSkeleton />
  if (team.isError) return <p className="text-sm text-muted-foreground">{team.error.message}</p>
  if (!me.data) return null

  const emails = memberEmails(team.data, invitations.data)
  emails.set(me.data.id, me.data.email)
  const actor = (team.data.members.find((m) => m.account_id === me.data!.id)?.role ?? "member") as TeamRole
  const admin = actor !== "member"
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
  const seats = billing.data && (billing.data.seats_bought ?? billing.data.paid_seats)
  const usedSeats = members.length + pending.length
  const freeSeats = billing.data?.seats_bought !== undefined ? billing.data.seats_bought - usedSeats : undefined
  const name = team.data.display_name || team.data.team_id

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
          <Button size="lg" onClick={() => setInviting(true)}>
            Invite members
          </Button>
        )}
      </header>

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
        ) : (
          <InvitationsTable team={team.data} invitations={shownInvites} emails={emails} me={me.data} />
        )}
      </section>

      {actor === "owner" && <TeamSettings team={team.data} />}
      <DangerZone team={team.data} owner={actor === "owner"} />
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

function TeamSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <Skeleton className="h-12 w-64" />
      <Skeleton className="h-48 w-full" />
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
  const menu = can.transfer || can.remove || can.leave
  return (
    <tr className="group hover:bg-surface [&>td]:h-14 [&>td]:px-3">
      <td>
        <Person email={email} name={member.display_name} you={you} />
      </td>
      <td>
        {can.changeRole ? (
          <DropdownMenu>
            <DropdownMenuTrigger className="flex items-center gap-1 rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30">
              {roleLabels[member.role]}
              <CaretDownIcon className="size-3" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="min-w-64">
              <DropdownMenuRadioGroup value={member.role} onValueChange={(next) => role.mutate(next as string)}>
                <DropdownMenuRadioItem value="admin">
                  <span className="flex flex-col">
                    Admin
                    <span className="text-xs text-muted-foreground">Invites, removes members, manages machines</span>
                  </span>
                </DropdownMenuRadioItem>
                <DropdownMenuRadioItem value="member">
                  <span className="flex flex-col">
                    Member
                    <span className="text-xs text-muted-foreground">Uses what the team gives them</span>
                  </span>
                </DropdownMenuRadioItem>
              </DropdownMenuRadioGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        ) : (
          <span className="text-muted-foreground">{roleLabels[member.role]}</span>
        )}
        {role.isError && <span className="block text-xs text-destructive">{role.error.message}</span>}
      </td>
      <td className="hidden text-muted-foreground md:table-cell">
        {member.joined_at ? <Time iso={member.joined_at} relative /> : "—"}
      </td>
      <td>
        {menu && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label={`Actions for ${who}`} />}>
              <DotsThreeIcon className="size-4" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {can.transfer && (
                <DropdownMenuItem onClick={() => setConfirm("transfer")}>Transfer ownership</DropdownMenuItem>
              )}
              {can.remove && (
                <DropdownMenuItem variant="destructive" onClick={() => setConfirm("remove")}>
                  Remove from team
                </DropdownMenuItem>
              )}
              {can.leave && (
                <DropdownMenuItem variant="destructive" onClick={() => setConfirm("leave")}>
                  Leave team
                </DropdownMenuItem>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        <ConfirmAction
          open={confirm === "remove"}
          onOpenChange={(open) => !open && setConfirm(null)}
          title={`Remove ${who}?`}
          description="They lose access to the team's machines, ENV and config right away. Their seat stays paid until the billing date and can be reused."
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
          description="They get billing and delete rights. You become a member. Only they can give ownership back."
          action="Transfer ownership"
          pending="Transferring…"
          mutation={{
            mutationFn: () => teamAction(team, "transfer", { account_id: member.account_id }),
            onSuccess: refresh,
          }}
        />
        {confirm === "leave" && <LeaveDialog team={team} onClose={() => setConfirm(null)} />}
      </td>
    </tr>
  )
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
  const revoke = useMutation({ mutationFn: (id: string) => cancelInvitation(team, id), onSuccess: refresh })
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        <col />
        <col className="w-28" />
        <col className="hidden w-48 md:table-column" />
        <col className="hidden w-32 md:table-column" />
        <col className="w-24" />
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
              </td>
              <td className="text-muted-foreground">{roleLabels[invitation.role] ?? invitation.role}</td>
              <td className="hidden truncate text-muted-foreground md:table-cell">{by}</td>
              <td className="hidden text-muted-foreground md:table-cell">
                <Time iso={invitation.expires_at} relative />
              </td>
              <td className="text-right">
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
      {revoke.isError && (
        <caption className="caption-bottom py-2 text-sm text-destructive">{revoke.error.message}</caption>
      )}
    </table>
  )
}

function TeamSettings({ team }: { team: Team }) {
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
  return (
    <Section title="Settings">
      <Row label="Team name" htmlFor="team-name">
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
      </Row>
      <Row label="Team ID">
        <span className="flex items-center gap-1 font-mono text-code">
          {team.team_id}
          <CopyButton value={team.team_id} label="Copy team ID" />
        </span>
      </Row>
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
      description="You lose access to its machines, ENV and config. You need a new invitation to come back."
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
    <section className="flex flex-col gap-3">
      <h2 className="text-sm font-medium text-destructive">Danger zone</h2>
      <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <p className="text-sm font-medium">{owner ? "Delete team" : "Leave team"}</p>
          <p className="text-sm text-muted-foreground">
            {owner
              ? "Members lose access, team ENV and config are deleted, team machines go back to their owners. Cancel the plan in Billing first."
              : "You lose access to the team's machines, ENV and config."}
          </p>
        </div>
        <Button
          variant="secondary"
          size="lg"
          className={cn(
            "bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15",
          )}
          onClick={() => setOpen(true)}
        >
          {owner ? "Delete team" : "Leave team"}
        </Button>
      </div>
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
    </section>
  )
}
