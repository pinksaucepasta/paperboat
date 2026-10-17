"use client"

import { DotsThreeIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { meQuery, useApiQuery, useWorkspace, workspacesQuery } from "@/lib/api"
import {
  answerInvitation,
  cancelInvitation,
  createTeam,
  inviteMember,
  myInvitationsQuery,
  teamAction,
  teamIdPattern,
  teamInvitationsQuery,
  teamQuery,
  type Team,
  type TeamInvitation,
  type TeamMember,
} from "@/lib/teams"
import { setWorkspace } from "@/lib/workspace"
import { Row, Section } from "@/components/section"
import { ConfirmAction } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"

const dateFormat = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" })
const roles = { owner: "Owner", admin: "Admin", member: "Member" } as Record<string, string>

export function TeamPage() {
  const workspace = useWorkspace()
  if (!workspace) return <Skeleton className="h-40 w-full" />
  return workspace.kind === "team" ? <TeamManage teamId={workspace.id} /> : <Personal />
}

/** In Personal: invitations waiting for you, and creating a team. */
function Personal() {
  const queryClient = useQueryClient()
  const invitations = useApiQuery(myInvitationsQuery)
  const [name, setName] = useState("")
  const create = useMutation({
    mutationFn: () => createTeam(name.trim()),
    onSuccess: async (team) => {
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
      setWorkspace(team.team_id)
    },
  })
  const answer = useMutation({
    mutationFn: ({ id, reply }: { id: string; reply: "accept" | "decline" }) => answerInvitation(id, reply),
    onSuccess: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: myInvitationsQuery.queryKey }),
        queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey }),
      ]),
  })
  const pending = invitations.data?.filter((invitation) => invitation.state === "pending") ?? []
  const valid = teamIdPattern.test(name.trim())
  return (
    <div className="flex flex-col gap-10">
      {pending.length > 0 && (
        <Section title="Invitations">
          {pending.map((invitation) => (
            <div
              key={invitation.invitation_id}
              className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2 text-sm"
            >
              <span>
                <span className="font-medium">{invitation.team_id}</span>
                <span className="text-muted-foreground"> · {roles[invitation.role] ?? invitation.role}</span>
              </span>
              <span className="flex gap-2">
                <Button
                  variant="outline"
                  disabled={answer.isPending}
                  onClick={() => answer.mutate({ id: invitation.invitation_id, reply: "decline" })}
                >
                  Decline
                </Button>
                <Button
                  disabled={answer.isPending}
                  onClick={() => answer.mutate({ id: invitation.invitation_id, reply: "accept" })}
                >
                  Join
                </Button>
              </span>
            </div>
          ))}
          {answer.isError && <p className="py-2 text-sm text-destructive">{answer.error.message}</p>}
        </Section>
      )}
      <Section title="Create a team">
        <form
          className="flex flex-col gap-2 py-3"
          onSubmit={(event) => {
            event.preventDefault()
            if (valid) create.mutate()
          }}
        >
          <div className="flex gap-2">
            <Input
              aria-label="Team name"
              placeholder="acme"
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
            <Button type="submit" disabled={!valid || create.isPending}>
              Create
            </Button>
          </div>
          <span className={name && !valid ? "text-sm text-destructive" : "text-sm text-muted-foreground"}>
            Lowercase letters, numbers and dashes. This is the team&apos;s ID and can&apos;t change.
          </span>
          {create.isError && <span className="text-sm text-destructive">{create.error.message}</span>}
        </form>
      </Section>
    </div>
  )
}

function TeamManage({ teamId }: { teamId: string }) {
  const team = useApiQuery(teamQuery(teamId))
  const invitations = useApiQuery(teamInvitationsQuery(teamId))
  const me = useApiQuery(meQuery)
  if (team.isPending || me.isPending) return <Skeleton className="h-40 w-full" />
  if (team.isError) return <p className="text-sm text-muted-foreground">{team.error.message}</p>
  if (!me.data) return null
  const self = team.data.members.find((member) => member.account_id === me.data.id)
  const role = self?.role ?? "member"
  const admin = role === "owner" || role === "admin"
  const pending = invitations.data?.filter((invitation) => invitation.state === "pending") ?? []
  const members = team.data.members.filter((member) => member.active)
  return (
    <div className="flex flex-col gap-10">
      <Section title="Members" action={<span className="text-xs text-muted-foreground">{members.length}</span>}>
        {members.map((member) => (
          <MemberRow
            key={member.account_id}
            team={team.data}
            member={member}
            you={member.account_id === me.data.id}
            actorRole={role}
          />
        ))}
      </Section>
      {admin && <Invite team={team.data} pending={pending} />}
      <DangerZone team={team.data} owner={role === "owner"} />
    </div>
  )
}

/** Refetches the team and its invitations after a change (every change bumps its generation). */
function useRefreshTeam(team: Team) {
  const queryClient = useQueryClient()
  return () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: teamQuery(team.team_id).queryKey }),
      queryClient.invalidateQueries({ queryKey: teamInvitationsQuery(team.team_id).queryKey }),
    ])
}

function MemberRow({
  team,
  member,
  you,
  actorRole,
}: {
  team: Team
  member: TeamMember
  you: boolean
  actorRole: string
}) {
  const [confirm, setConfirm] = useState<"remove" | "transfer" | null>(null)
  const refresh = useRefreshTeam(team)
  const role = useMutation({
    mutationFn: () =>
      teamAction(team, "role", { account_id: member.account_id, role: member.role === "admin" ? "member" : "admin" }),
    onSuccess: refresh,
  })
  const remove = {
    mutationFn: () => teamAction(team, "remove", { account_id: member.account_id }),
    onSuccess: refresh,
  }
  const transfer = {
    mutationFn: () => teamAction(team, "transfer", { account_id: member.account_id }),
    onSuccess: refresh,
  }
  const owner = actorRole === "owner"
  const canRemove = !you && member.role !== "owner" && (owner || (actorRole === "admin" && member.role === "member"))
  const canRole = owner && !you && member.role !== "owner"
  return (
    <div className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2 text-sm">
      <span className="min-w-0 truncate">
        {member.account_id}
        {you && <span className="text-muted-foreground"> (you)</span>}
      </span>
      <span className="flex items-center gap-3">
        <span className="text-muted-foreground">{roles[member.role]}</span>
        {(canRemove || canRole) && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label="Member actions" />}>
              <DotsThreeIcon className="size-4" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {canRole && (
                <DropdownMenuItem onClick={() => role.mutate()}>
                  {member.role === "admin" ? "Make member" : "Make admin"}
                </DropdownMenuItem>
              )}
              {canRole && (
                <DropdownMenuItem onClick={() => setConfirm("transfer")}>Transfer ownership</DropdownMenuItem>
              )}
              {canRemove && (
                <DropdownMenuItem variant="destructive" onClick={() => setConfirm("remove")}>
                  Remove from team
                </DropdownMenuItem>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </span>
      <ConfirmAction
        open={confirm === "remove"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Remove from team?"
        description="They lose access to the team's machines, ENV and config right away. Their own machines stay theirs."
        action="Remove"
        pending="Removing…"
        mutation={remove}
      />
      <ConfirmAction
        open={confirm === "transfer"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Transfer ownership?"
        description="They become the owner, with billing and delete rights. You become a member."
        action="Transfer"
        pending="Transferring…"
        mutation={transfer}
      />
      {role.isError && <span className="text-destructive">{role.error.message}</span>}
    </div>
  )
}

function Invite({ team, pending }: { team: Team; pending: TeamInvitation[] }) {
  const [email, setEmail] = useState("")
  const refresh = useRefreshTeam(team)
  const invite = useMutation({
    mutationFn: () => inviteMember(team, email.trim()),
    onSuccess: async () => {
      setEmail("")
      await refresh()
    },
  })
  const cancel = useMutation({ mutationFn: (id: string) => cancelInvitation(team, id), onSuccess: refresh })
  return (
    <Section title="Invitations">
      <form
        className="flex gap-2 border-b border-divider py-3"
        onSubmit={(event) => {
          event.preventDefault()
          if (email.includes("@")) invite.mutate()
        }}
      >
        <Input
          type="email"
          aria-label="Email"
          placeholder="name@company.com"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
        />
        <Button type="submit" disabled={!email.includes("@") || invite.isPending}>
          Invite
        </Button>
      </form>
      {invite.isError && <p className="py-2 text-sm text-destructive">{invite.error.message}</p>}
      {pending.map((invitation) => (
        <Row key={invitation.invitation_id} label={invitation.email ?? invitation.account_id}>
          <span className="flex items-center gap-3">
            <span className="text-muted-foreground">Expires {dateFormat.format(new Date(invitation.expires_at))}</span>
            <Button
              variant="outline"
              disabled={cancel.isPending}
              onClick={() => cancel.mutate(invitation.invitation_id)}
            >
              Cancel
            </Button>
          </span>
        </Row>
      ))}
      {cancel.isError && <p className="py-2 text-sm text-destructive">{cancel.error.message}</p>}
    </Section>
  )
}

/** Leaving (members and admins) and deleting (owner only, after billing is cancelled). */
function DangerZone({ team, owner }: { team: Team; owner: boolean }) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const done = async () => {
    setWorkspace("personal")
    await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
  }
  const mutation = owner
    ? { mutationFn: () => teamAction(team, "delete", { confirmation: team.team_id }), onSuccess: done }
    : { mutationFn: () => teamAction(team, "leave"), onSuccess: done }
  return (
    <Section title="Danger zone">
      <div className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2 text-sm">
        <span className="text-muted-foreground">
          {owner
            ? "Delete the team. Cancel its subscription in Billing first."
            : "Leave the team and lose access to its machines."}
        </span>
        <Button variant="destructive" onClick={() => setOpen(true)}>
          {owner ? "Delete team" : "Leave team"}
        </Button>
      </div>
      <ConfirmAction
        open={open}
        onOpenChange={setOpen}
        title={owner ? "Delete this team?" : "Leave this team?"}
        description={
          owner
            ? "Members lose access, team ENV and config are deleted, and team machines go back to their owners."
            : "You need a new invitation to come back."
        }
        action={owner ? "Delete" : "Leave"}
        pending={owner ? "Deleting…" : "Leaving…"}
        mutation={mutation}
        confirmName={owner ? team.team_id : undefined}
      />
    </Section>
  )
}
