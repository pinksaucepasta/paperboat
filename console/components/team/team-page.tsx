"use client"

import { CircleNotchIcon, EnvelopeSimpleIcon, MinusIcon, PlusIcon, UsersThreeIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useRouter, useSearchParams } from "next/navigation"
import { useEffect, useMemo, useState, useSyncExternalStore } from "react"
import { idempotencyKey, meQuery, needsSso, useApiQuery, useWorkspace, workspacesQuery } from "@/lib/api"
import { billingLive, billingQuery, notLiveTitle, teamTrialDays } from "@/lib/billing"
import {
  answerInvitation,
  createPaidTeam,
  createTeam,
  myInvitationsQuery,
  roleLabels,
  teamIdPattern,
} from "@/lib/teams"
import { setWorkspace } from "@/lib/workspace"
import { TeamManage } from "@/components/team/team-members"
import { TeamsUpgradeDialog } from "@/components/team/teams-upgrade"
import { Time } from "@/components/time"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"


export function TeamPage() {
  const workspace = useWorkspace()
  // `?team=<id>` (the WorkOS Admin Portal returns to /team?team=<id>&tab=security) opens that
  // team when it is one of yours; the server still checks every request.
  const wanted = useSearchParams().get("team")
  const workspaces = useApiQuery(workspacesQuery).data
  useEffect(() => {
    if (wanted && wanted !== workspace?.id && workspaces?.some((item) => item.kind === "team" && item.id === wanted))
      setWorkspace(wanted)
  }, [wanted, workspace?.id, workspaces])
  if (!workspace) return <Skeleton className="h-40 w-full" />
  return workspace.kind === "team" ? <TeamManage workspace={workspace} /> : <Teams />
}

/**
 * Back from Checkout (`?created=<team>`): the team exists once the payment webhook lands, a moment
 * after the redirect. Refreshes the workspace list until it shows up, then opens the team.
 * Returns the team being confirmed, or undefined.
 */
function useConfirmedTeam(workspaceIds: string[] | undefined) {
  const queryClient = useQueryClient()
  const created = useSyncExternalStore(
    () => () => {},
    () => new URLSearchParams(window.location.search).get("created"),
    () => null,
  )
  const [gaveUp, setGaveUp] = useState(false)
  const arrived = Boolean(created && workspaceIds?.includes(created))
  useEffect(() => {
    if (!created) return
    if (arrived) {
      window.history.replaceState(null, "", "/team")
      setWorkspace(created)
      return
    }
    const timer = setInterval(() => queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey }), 2000)
    // Stop waiting after a minute; the team still appears in the list once it exists.
    const stop = setTimeout(() => setGaveUp(true), 60_000)
    return () => {
      clearInterval(timer)
      clearTimeout(stop)
    }
  }, [created, arrived, queryClient])
  return created && !arrived && !gaveUp ? created : undefined
}

/** In Personal: the teams you belong to, invitations waiting for you, and creating a team. */
function Teams() {
  const workspaces = useApiQuery(workspacesQuery)
  const confirming = useConfirmedTeam(workspaces.data?.map((workspace) => workspace.id))
  const invitations = useApiQuery(myInvitationsQuery)
  // `?create=1` (from the Teams prompt elsewhere) opens Create team once.
  const router = useRouter()
  const createRequested = useSearchParams().get("create") === "1"
  const [creating, setCreating] = useState(false)
  const [adding, setAdding] = useState(false)
  // Handled once per arrival: closing the dialog must not reopen it before the URL is cleaned.
  const [handledCreate, setHandledCreate] = useState(false)
  if (createRequested && !handledCreate) {
    setHandledCreate(true)
    setCreating(true)
  }
  if (!createRequested && handledCreate) setHandledCreate(false)
  useEffect(() => {
    if (createRequested) router.replace("/team", { scroll: false })
  }, [createRequested, router])
  const teams = workspaces.data?.filter((workspace) => workspace.kind === "team") ?? []
  const me = useApiQuery(meQuery).data
  // The list also holds invitations you sent; only those addressed to you belong here.
  const pending =
    invitations.data?.filter(
      (invitation) =>
        invitation.state === "pending" &&
        me &&
        invitation.created_by !== me.id &&
        (invitation.account_id === me.id || invitation.email?.toLowerCase() === me.email.toLowerCase()),
    ) ?? []

  return (
    <div className="flex flex-col gap-10">
      <header className="flex items-center gap-4">
        <h1 className="flex-1 font-heading text-2xl font-semibold tracking-tight">Teams</h1>
        <Button variant="outline" size="lg" onClick={() => setAdding(true)}>
          Add member
        </Button>
        <Button size="lg" onClick={() => setCreating(true)}>
          Create team
        </Button>
      </header>

      {pending.length > 0 && (
        <section className="flex flex-col gap-3">
          <h2 className="text-sm font-medium">Invitations</h2>
          {pending.map((invitation) => (
            <InvitationCard
              key={invitation.invitation_id}
              id={invitation.invitation_id}
              team={invitation.team_id}
              role={invitation.role}
              expires={invitation.expires_at}
            />
          ))}
        </section>
      )}

      {confirming && (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <CircleNotchIcon className="size-4 animate-spin" /> Confirming payment for {confirming}…
        </p>
      )}

      <section className="flex flex-col gap-3">
        <h2 className="text-sm font-medium">Your teams</h2>
        {workspaces.isPending ? (
          <Skeleton className="h-28 w-full" />
        ) : teams.length === 0 ? (
          <div className="flex flex-col items-center gap-3 surface rounded-lg px-6 py-12 text-center">
            <UsersThreeIcon className="size-8 text-muted-foreground" />
            <p className="font-medium">Work on machines together</p>
            <p className="max-w-sm text-sm text-muted-foreground">
              Share machines, ENV and config with your team, with roles and seats billed together.
            </p>
            <Button size="lg" onClick={() => setCreating(true)}>
              Create team
            </Button>
          </div>
        ) : (
          <div className="flex flex-col divide-y divide-divider border-y border-divider">
            {teams.map((team) => (
              <button
                key={team.id}
                type="button"
                onClick={() => setWorkspace(team.id)}
                className="flex h-16 items-center gap-3 px-3 text-left text-sm hover:bg-surface"
              >
                <Avatar className="size-9 rounded-md">
                  <AvatarFallback className="rounded-md bg-primary/12 font-semibold text-primary">
                    {team.name.slice(0, 1).toUpperCase()}
                  </AvatarFallback>
                </Avatar>
                <span className="flex min-w-0 flex-1 flex-col">
                  <span className="truncate font-medium">{team.name}</span>
                  <span className="truncate text-xs text-muted-foreground">
                    {needsSso(team)
                      ? "Sign in with your work email to open this team"
                      : `${roleLabels[team.role]}${team.name !== team.id ? ` · ${team.id}` : ""}`}
                  </span>
                </span>
                <span className="text-muted-foreground">Open</span>
              </button>
            ))}
          </div>
        )}
      </section>

      <CreateTeamDialog open={creating} onOpenChange={setCreating} />
      <TeamsUpgradeDialog reason="member" open={adding} onOpenChange={setAdding} />
    </div>
  )
}

function InvitationCard({ id, team, role, expires }: { id: string; team: string; role: string; expires: string }) {
  const queryClient = useQueryClient()
  const answer = useMutation({
    mutationFn: (reply: "accept" | "decline") => answerInvitation(id, reply),
    onSuccess: async (_, reply) => {
      await queryClient.invalidateQueries({ queryKey: myInvitationsQuery.queryKey })
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
      if (reply === "accept") setWorkspace(team)
    },
  })
  return (
    <div className="flex flex-wrap items-center gap-4 surface rounded-lg p-4">
      <EnvelopeSimpleIcon className="size-5 text-primary" />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 text-sm">
        <span>
          You&apos;re invited to join <span className="font-medium">{team}</span> as{" "}
          {roleLabels[role]?.toLowerCase() ?? role}
        </span>
        <span className="text-muted-foreground">
          Expires <Time iso={expires} relative />
        </span>
        {answer.isError && <span className="text-destructive">{answer.error.message}</span>}
      </div>
      <Button variant="outline" size="lg" disabled={answer.isPending} onClick={() => answer.mutate("decline")}>
        Decline
      </Button>
      <Button size="lg" disabled={answer.isPending} onClick={() => answer.mutate("accept")}>
        Join team
      </Button>
    </div>
  )
}

function CreateTeamDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create team</DialogTitle>
        </DialogHeader>
        {open && <CreateTeamForm />}
      </DialogContent>
    </Dialog>
  )
}

/** Creating a team is paying first (NOTES.md "Teams"); until that endpoint is live it is free. */
function CreateTeamForm() {
  const queryClient = useQueryClient()
  const [id, setId] = useState("")
  const [seats, setSeats] = useState(1)
  const [period, setPeriod] = useState<"month" | "year">("month")
  const paid = billingLive.paidTeamCreate
  const personal = useApiQuery(workspacesQuery).data?.find((workspace) => workspace.kind === "personal")
  const trial = teamTrialDays(useApiQuery(billingQuery(personal)).data)
  // One operation per distinct order: changing the team, seats or interval is a new order.
  const key = useMemo(() => idempotencyKey(), [id, seats, period]) // eslint-disable-line react-hooks/exhaustive-deps
  const valid = teamIdPattern.test(id)
  const create = useMutation({
    mutationFn: async () => {
      if (paid) return window.location.assign(await createPaidTeam(id, seats, period, key))
      const team = await createTeam(id)
      await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
      setWorkspace(team.team_id)
    },
  })
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (valid) create.mutate()
      }}
    >
      <Field className="gap-1.5">
        <FieldLabel htmlFor="team-id">Team ID</FieldLabel>
        <Input
          id="team-id"
          autoFocus
          placeholder="acme"
          value={id}
          onChange={(event) => setId(event.target.value.toLowerCase().replace(/[^a-z0-9-]/g, "-"))}
          autoComplete="off"
          spellCheck={false}
        />
        <span className="text-xs text-muted-foreground">
          Lowercase letters, numbers and dashes. It can&apos;t change later.
        </span>
      </Field>
      <Field className="gap-1.5" title={paid ? undefined : notLiveTitle}>
        <FieldLabel>Seats</FieldLabel>
        <div className="flex flex-wrap items-center gap-3">
          <span className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label="Fewer seats"
              disabled={!paid || seats <= 1}
              onClick={() => setSeats(seats - 1)}
            >
              <MinusIcon />
            </Button>
            <span className="w-6 text-center tabular-nums">{seats}</span>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label="More seats"
              disabled={!paid}
              onClick={() => setSeats(seats + 1)}
            >
              <PlusIcon />
            </Button>
          </span>
          <ToggleGroup
            value={[period]}
            onValueChange={(next) => next[0] && setPeriod(next[0] as "month" | "year")}
            variant="outline"
            spacing={0}
            disabled={!paid}
          >
            <ToggleGroupItem value="month">Monthly</ToggleGroupItem>
            <ToggleGroupItem value="year">Yearly</ToggleGroupItem>
          </ToggleGroup>
        </div>
        <span className="text-xs text-muted-foreground">
          {paid && trial
            ? `You take one seat. Free for ${trial} days with your card on file; billing begins on day ${trial + 1}, and cancelling before then costs nothing.`
            : paid
            ? "You take one seat. Add more any time; new seats are charged for the rest of the period."
            : `${notLiveTitle}: teams are created free for now.`}
        </span>
      </Field>
      {create.isError && <p className="text-sm text-destructive">{create.error.message}</p>}
      <DialogFooter>
        <Button type="submit" size="lg" disabled={!valid || create.isPending}>
          {create.isPending
            ? "Creating…"
            : paid && trial
              ? `Start ${trial}-day free trial`
              : paid
                ? "Continue to payment"
                : "Create team"}
        </Button>
      </DialogFooter>
    </form>
  )
}
