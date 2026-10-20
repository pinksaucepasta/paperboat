"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { grantMachine, machineCapabilities, teamQuery, type Team } from "@/lib/teams"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"

type Grant = NonNullable<Team["machine_grants"]>[number]
type Audience = { account: string }

const everyone: Audience = { account: "" }

/** The grant for one audience: the whole team (`account` empty) or one member. */
function grantFor(team: Team, machineId: string, audience: Audience): Grant | undefined {
  return team.machine_grants?.find(
    (grant) =>
      grant.machine_id === machineId &&
      (audience.account
        ? grant.audience === "selected_member" && grant.account_id === audience.account
        : grant.audience === "all_members"),
  )
}

function grantedLabels(grant: Grant | undefined) {
  if (!grant?.active) return []
  return machineCapabilities.filter((c) => grant.capabilities.includes(c.id)).map((c) => c.label)
}

/**
 * What members may do on the team's machines. Owners and admins (and a machine's owner) choose, per
 * machine, the actions the whole team or one member may use; nothing is allowed until it is granted.
 */
export function MachineAccess({
  team,
  admin,
  accountId,
  emails,
}: {
  team: Team
  admin: boolean
  accountId: string | undefined
  emails: Map<string, string>
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const machines = (team.machines ?? []).filter((m) => m.active && (admin || m.owner_account === accountId))
  if (!machines.length) return null
  const machine = machines.find((m) => m.machine_id === editing)
  return (
    <Section
      title="Machine access"
      info={{
        title: "Who can use a machine",
        body: "A machine in the team is not usable by members until you grant it. Grant the whole team, or one member, only what they need.",
      }}
    >
      <ul className="flex flex-col">
        {machines.map((m) => {
          const labels = grantedLabels(grantFor(team, m.machine_id, everyone))
          const members = (team.machine_grants ?? []).filter(
            (g) => g.machine_id === m.machine_id && g.audience === "selected_member" && g.active,
          ).length
          return (
            <li key={m.machine_id} className="flex min-h-12 flex-wrap items-center gap-3 py-2 text-sm">
              <span className="min-w-0 flex-1">
                <span className="font-medium">{m.alias}</span>
                <span className="block truncate text-muted-foreground">
                  {labels.length ? `Team: ${labels.join(", ")}` : "No access for the team"}
                  {members > 0 && ` · ${members} ${members === 1 ? "member" : "members"} with their own access`}
                </span>
              </span>
              <Button variant="outline" onClick={() => setEditing(m.machine_id)}>
                Manage access
              </Button>
            </li>
          )
        })}
      </ul>
      <Dialog open={Boolean(machine)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent className="gap-4 sm:max-w-md">
          {machine && (
            <AccessForm
              team={team}
              machineId={machine.machine_id}
              alias={machine.alias}
              emails={emails}
              onDone={() => setEditing(null)}
            />
          )}
        </DialogContent>
      </Dialog>
    </Section>
  )
}

function AccessForm({
  team,
  machineId,
  alias,
  emails,
  onDone,
}: {
  team: Team
  machineId: string
  alias: string
  emails: Map<string, string>
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  const [account, setAccount] = useState("")
  const audience: Audience = { account }
  const current = grantFor(team, machineId, audience)
  const [edits, setEdits] = useState<Record<string, string[]>>({})
  const have: string[] = current?.active ? current.capabilities : []
  const checked = edits[account] ?? have
  const people = team.members.filter((m) => m.active)

  const save = useMutation({
    mutationFn: () => {
      const chosen = machineCapabilities.map((c) => c.id as string).filter((id) => checked.includes(id))
      // The server needs at least one capability even for a grant it switches off, so removing all
      // access sends the set the grant had.
      const kept = chosen.length ? chosen : (current?.capabilities ?? [])
      return grantMachine(team, machineId, audience, kept, chosen.length > 0)
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: teamQuery(team.team_id).queryKey })
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
      onDone()
    },
  })
  const unchanged = checked.length === have.length && checked.every((id) => have.includes(id))
  const nothingToRemove = !checked.length && !current
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        save.mutate()
      }}
    >
      <DialogHeader>
        <DialogTitle>Access to {alias}</DialogTitle>
        <DialogDescription>
          Members can use only what is ticked. Access for one member is added to what the whole team has.
        </DialogDescription>
      </DialogHeader>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="machine-access-audience">Who</FieldLabel>
        <select
          id="machine-access-audience"
          className="h-9 rounded-md border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
          value={account}
          onChange={(event) => setAccount(event.target.value)}
        >
          <option value="">Everyone in the team</option>
          {people.map((member) => (
            <option key={member.account_id} value={member.account_id}>
              {emails.get(member.account_id) ?? member.display_name ?? member.account_id}
            </option>
          ))}
        </select>
      </Field>
      <ul className="flex flex-col gap-3">
        {machineCapabilities.map((capability) => (
          <li key={capability.id}>
            <label className="flex cursor-pointer items-start gap-2.5 text-sm">
              <Checkbox
                className="mt-0.5"
                checked={checked.includes(capability.id)}
                onCheckedChange={(on) =>
                  setEdits({
                    ...edits,
                    [account]: on ? [...checked, capability.id] : checked.filter((id) => id !== capability.id),
                  })
                }
              />
              <span className="flex flex-col">
                <span className="font-medium">{capability.label}</span>
                <span className="text-xs text-muted-foreground">{capability.hint}</span>
              </span>
            </label>
          </li>
        ))}
      </ul>
      {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
      <DialogFooter>
        <Button type="submit" size="lg" disabled={save.isPending || unchanged || nothingToRemove}>
          {save.isPending ? "Saving…" : "Save access"}
        </Button>
      </DialogFooter>
    </form>
  )
}
