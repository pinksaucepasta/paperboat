"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState, useSyncExternalStore } from "react"
import { useApiQuery, workspacesQuery, type Workspace } from "@/lib/api"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import {
  acceptTeamKeys,
  createTeamVault,
  grantTeamKey,
  readTeamVault,
  rotateTeamVault,
  type VaultTeam,
} from "@/lib/envvault/manage"
import { teamQuery } from "@/lib/teams"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage } from "@/components/error-reference"

/** Each team's variables are sealed with the team's own key: create it, give members the key, replace it. */
export function EnvTeamVaults() {
  const teams = (useApiQuery(workspacesQuery).data ?? []).filter((workspace) => workspace.kind === "team")
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  if (teams.length === 0) return null
  return (
    <Section title="Team vaults" action={unlocked ? <AcceptKeys /> : undefined}>
      {teams.map((team) => (
        <TeamRow key={team.id} team={team} unlocked={unlocked} />
      ))}
    </Section>
  )
}

/** Keys other members gave this account are taken into the vault and acknowledged. */
function AcceptKeys() {
  const queryClient = useQueryClient()
  const accept = useMutation({
    mutationFn: acceptTeamKeys,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["env-team"] }),
  })
  return (
    <div className="flex items-center gap-3">
      {accept.isError && (
        <span className="text-sm text-destructive">
          <ErrorMessage error={accept.error} />
        </span>
      )}
      {accept.isSuccess && (
        <span className="text-sm text-muted-foreground">
          {accept.data === 0 ? "No new keys." : `${accept.data} accepted.`}
        </span>
      )}
      <Button variant="outline" size="lg" disabled={accept.isPending} onClick={() => accept.mutate()}>
        {accept.isPending ? "Checking…" : "Accept keys"}
      </Button>
    </div>
  )
}

type Action = "grant" | "remove" | "rotate" | "reset" | "create" | null

function TeamRow({ team, unlocked }: { team: Workspace; unlocked: boolean }) {
  const queryClient = useQueryClient()
  const vault = useApiQuery({ queryKey: ["env-team", team.id], queryFn: ({ signal }) => readTeamVault(team.id, signal) })
  const [dialog, setDialog] = useState<Action>(null)
  const owner = team.role === "owner"
  const admin = owner || team.role === "admin"
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["env-team"] })
  const state = vault.data
  const created = Boolean(state && state.key_epoch > 0)
  const behind = state?.members.filter((member) => member.active && member.env_permission && member.grant_epoch !== state.key_epoch).length ?? 0
  return (
    <SettingRow
      label={team.name}
      description={
        vault.isPending ? (
          <Skeleton className="my-0.5 h-4 w-40" />
        ) : vault.isError ? (
          <ErrorMessage error={vault.error} />
        ) : !created ? (
          "No variables yet."
        ) : (
          [
            behind > 0 && `${behind} ${behind === 1 ? "member needs" : "members need"} the key.`,
            behind === 0 && "Key in use.",
          ]
            .filter(Boolean)
            .join(" ")
        )
      }
    >
      {admin && !vault.isPending && (
        <div className="flex flex-wrap justify-end gap-2">
          {created ? (
            <>
              <Button variant="outline" size="sm" disabled={!unlocked} onClick={() => setDialog("grant")}>
                Give key…
              </Button>
              <Button variant="outline" size="sm" disabled={!unlocked} onClick={() => setDialog("remove")}>
                Remove access…
              </Button>
              <Button variant="outline" size="sm" disabled={!unlocked} onClick={() => setDialog("rotate")}>
                New key…
              </Button>
              {owner && (
                <Button variant="outline" size="sm" disabled={!unlocked} onClick={() => setDialog("reset")}>
                  Reset…
                </Button>
              )}
            </>
          ) : (
            owner && (
              <Button variant="outline" size="sm" disabled={!unlocked} onClick={() => setDialog("create")}>
                Create…
              </Button>
            )
          )}
        </div>
      )}
      {dialog === "create" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setDialog(null)}
          title={`Create ${team.name}’s vault?`}
          description="Its variables are sealed with a key only team members hold. Give members the key afterwards."
          action="Create"
          pending="Creating…"
          mutation={{ mutationFn: () => createTeamVault(team.id), onSuccess: refresh }}
        />
      )}
      {dialog === "grant" && state && (
        <MemberPicker
          title="Give the team key"
          team={team}
          state={state}
          action="Give key"
          members={state.members.filter((member) => member.active && member.env_permission && member.grant_epoch !== state.key_epoch)}
          empty="Every member with access already has the key."
          run={(account) => grantTeamKey(team.id, account)}
          onClose={() => setDialog(null)}
          onDone={refresh}
        />
      )}
      {dialog === "remove" && state && (
        <MemberPicker
          title="Remove access"
          team={team}
          state={state}
          action="Remove"
          confirmName={team.id}
          members={state.members.filter((member) => member.active && member.env_permission && (owner || member.role === "member"))}
          empty="No members to remove."
          run={(account) => rotateTeamVault(team.id, { remove: [account] })}
          onClose={() => setDialog(null)}
          onDone={refresh}
        />
      )}
      {dialog === "rotate" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setDialog(null)}
          title={`Replace ${team.name}’s key?`}
          description="The variables are sealed again with a new key, and every member keeps access."
          action="Replace key"
          pending="Replacing…"
          confirmName={team.id}
          mutation={{ mutationFn: () => rotateTeamVault(team.id), onSuccess: refresh }}
        />
      )}
      {dialog === "reset" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setDialog(null)}
          title={`Reset ${team.name}’s vault?`}
          description="Every team variable is deleted, then the vault starts again with a new key. This can’t be undone."
          action="Reset"
          pending="Resetting…"
          confirmName={`RESET ${team.id}`}
          mutation={{ mutationFn: () => rotateTeamVault(team.id, { lostKey: true }), onSuccess: refresh }}
        />
      )}
    </SettingRow>
  )
}

/** One member picked from the list; `confirmName` asks for the team ID first (the change replaces the key). */
function MemberPicker({
  title,
  team,
  state,
  members,
  action,
  confirmName,
  empty,
  run,
  onClose,
  onDone,
}: {
  title: string
  team: Workspace
  state: VaultTeam
  members: VaultTeam["members"]
  action: string
  confirmName?: string
  empty: string
  run: (account: string) => Promise<unknown>
  onClose: () => void
  onDone: () => void
}) {
  const people = useApiQuery(teamQuery(team.id)).data?.members
  const [typed, setTyped] = useState("")
  const go = useMutation({
    mutationFn: run,
    onSuccess: () => {
      onDone()
      onClose()
    },
  })
  const ready = !confirmName || typed.trim() === confirmName
  return (
    <Dialog open onOpenChange={(open) => !open && !go.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>{title}</DialogTitle>
        {confirmName && (
          <label className="flex flex-col gap-1.5 text-sm text-muted-foreground">
            Type <span className="font-mono font-medium text-foreground">{confirmName}</span> to confirm
            <input
              className="h-9 rounded-md border border-input bg-transparent px-2.5 text-foreground outline-none focus-visible:border-ring"
              value={typed}
              autoComplete="off"
              onChange={(event) => setTyped(event.target.value)}
            />
          </label>
        )}
        <div className="flex flex-col">
          {members.length === 0 && <p className="py-3 text-sm text-muted-foreground">{empty}</p>}
          {members
            .filter((member) => member.account_id !== state.owner_account || confirmName === undefined)
            .map((member) => (
              <div key={member.account_id} className="flex min-h-12 items-center justify-between gap-3 border-b border-divider text-sm">
                <span className="min-w-0 truncate">
                  {people?.find((entry) => entry.account_id === member.account_id)?.display_name || member.account_id}
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={go.isPending || !ready}
                  onClick={() => go.mutate(member.account_id)}
                >
                  {go.isPending && go.variables === member.account_id ? "Working…" : action}
                </Button>
              </div>
            ))}
        </div>
        {go.isError && (
          <p className="text-sm text-destructive">
            <ErrorMessage error={go.error} />
          </p>
        )}
      </DialogContent>
    </Dialog>
  )
}
