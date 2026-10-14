"use client"

import { CircleNotchIcon, PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import {
  api,
  idempotencyKey,
  machinesQuery,
  meQuery,
  useApiQuery,
  useMachines,
  workspacesQuery,
  type Machine,
  type Team,
  type Workspace,
} from "@/lib/api"
import { useMachineDomains } from "@/lib/machine-domains"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * A team's "Add machine": machines join a team by being shared from their owner's personal
 * machines (`POST /v1/teams/{team}/machines`, action `share`), so this lists yours to pick from.
 */
export function ShareMachineDialog({ team }: { team: Workspace }) {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="lg" />}>
        <PlusIcon data-icon="inline-start" />
        Add machine
      </DialogTrigger>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a machine to {team.name}</DialogTitle>
        </DialogHeader>
        {open && <ShareMachine team={team} onDone={() => setOpen(false)} />}
      </DialogContent>
    </Dialog>
  )
}

function ShareMachine({ team, onDone }: { team: Workspace; onDone: () => void }) {
  const queryClient = useQueryClient()
  const { primary: primaryDomain } = useMachineDomains()
  const { data: me } = useApiQuery(meQuery)
  const { data: workspaces } = useApiQuery(workspacesQuery)
  const personalId = workspaces?.find((workspace) => workspace.kind === "personal")?.id
  const personal = useApiQuery({ ...machinesQuery(personalId ?? ""), enabled: Boolean(personalId) })
  const inTeam = useMachines()
  const already = new Set(inTeam.data?.map((machine) => machine.id))
  const yours = personal.data?.items.filter(
    (machine) => machine.owner_account === me?.id && !machine.owner_team_id && !already.has(machine.id),
  )

  const share = useMutation({
    mutationFn: async (machine: Machine) => {
      const path = `teams/${encodeURIComponent(team.id)}`
      // Team changes are fenced by the team's generation, read just before the change.
      const current = await api<Team>(path)
      return api(`${path}/machines`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          operation_id: idempotencyKey(),
          expected_generation: current.generation,
          machine_id: machine.id,
          action: "share",
        }),
      })
    },
    onSuccess: async (_, machine) => {
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
      toast.success(`${machine.alias} added to ${team.name}`)
      onDone()
    },
  })

  if (personal.isPending || !me) {
    return (
      <div className="flex flex-col gap-1">
        {Array.from({ length: 3 }, (_, index) => (
          <Skeleton key={index} className="h-11" />
        ))}
      </div>
    )
  }
  if (personal.isError) return <p className="text-sm text-muted-foreground">Couldn’t load your machines.</p>
  if (!yours?.length) {
    return (
      <p className="text-sm text-muted-foreground">
        All your machines are already here. Add a new one in Personal first, then add it to {team.name}.
      </p>
    )
  }

  return (
    <div className="flex flex-col gap-3">
      <ul className="-mx-2 flex max-h-80 flex-col overflow-y-auto">
        {yours.map((machine) => (
          <li key={machine.id}>
            <button
              type="button"
              disabled={share.isPending}
              onClick={() => share.mutate(machine)}
              className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30 disabled:opacity-60"
            >
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="flex h-[1lh] items-center gap-2 text-sm font-medium">
                  <span className="trim-cap">{machine.alias}</span>
                  <StatusDot online={machine.online} />
                </span>
                <span className="truncate font-mono text-xs text-muted-foreground">{`${machine.alias}.${primaryDomain}`}</span>
              </span>
              {share.isPending && share.variables?.id === machine.id ? (
                <CircleNotchIcon className="size-4 animate-spin text-muted-foreground" />
              ) : (
                <span className="text-sm text-muted-foreground">Add</span>
              )}
            </button>
          </li>
        ))}
      </ul>
      {share.isError && <p className="text-sm text-destructive">{share.error.message}</p>}
    </div>
  )
}
