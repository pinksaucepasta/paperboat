"use client"

import { CheckCircleIcon, CircleNotchIcon, PlusIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { useMachines, useWorkspace, type Machine } from "@/lib/api"
import { createTunnel, hostOf, tunnelQuery, type Tunnel } from "@/lib/tunnels"
import { useNow } from "@/lib/use-now"
import { CopyRow } from "@/components/copy-button"
import { canPreview, MachineChoice, usePort } from "@/components/tunnels/machine-choice"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

/** A tunnel name is kept to lowercase letters, digits and dashes, so it reads well in its address. */
const slug = (value: string) =>
  value
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+/, "")
    .slice(0, 63)

export function NewTunnelButton({ machine }: { machine?: Machine }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size="lg" onClick={() => setOpen(true)}>
        <PlusIcon data-icon="inline-start" />
        New tunnel
      </Button>
      {/* Mounted only while open, so each opening starts from a clean form. */}
      {open && <NewTunnelDialog machine={machine} onClose={() => setOpen(false)} />}
    </>
  )
}

/**
 * Name it, pick the machine and port, create. The machine's Paperboat service attaches the
 * tunnel, and the dialog stays open until it's connected, then hands you its address. More
 * hostnames, your own domains included, are added on the tunnel's page.
 */
function NewTunnelDialog({ machine, onClose }: { machine?: Machine; onClose: () => void }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const machines = useMachines()
  const [chosen, setChosen] = useState<Machine | undefined>(machine)
  const target = chosen ?? machines.data?.find(canPreview)
  const [name, setName] = useState("")
  const port = usePort()
  const [access, setAccess] = useState<Tunnel["access_mode"]>("public")
  const nameValue = name.replace(/-+$/, "")

  const create = useMutation({
    mutationFn: () =>
      createTunnel({
        machineId: target!.id,
        workspace: workspace?.id ?? "personal",
        name: nameValue,
        port: port.value,
        access,
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["tunnels"] }),
  })
  const ready = Boolean(nameValue && port.value && target && canPreview(target))

  return (
    <Dialog open onOpenChange={(next) => !next && !create.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New tunnel</DialogTitle>
        </DialogHeader>
        {create.data ? (
          <Connecting id={create.data} machineName={target?.alias ?? "the machine"} onDone={onClose} />
        ) : (
          <form
            className="flex min-w-0 flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (ready) create.mutate()
            }}
          >
            <Field className="gap-1.5">
              <FieldLabel htmlFor="tunnel-name">Name</FieldLabel>
              <Input
                id="tunnel-name"
                autoFocus
                placeholder="coolify"
                value={name}
                onChange={(event) => setName(slug(event.target.value))}
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
            <div className="grid grid-cols-[minmax(0,1fr)_6rem] gap-3">
              <Field className="min-w-0 gap-1.5">
                <FieldLabel>Machine</FieldLabel>
                <MachineChoice value={target} onChange={setChosen} usable={canPreview} />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor="tunnel-port">Port</FieldLabel>
                <Input
                  id="tunnel-port"
                  inputMode="numeric"
                  value={port.text}
                  onChange={(event) => port.set(event.target.value)}
                  aria-invalid={port.text !== "" && !port.value}
                  className="font-mono"
                />
              </Field>
            </div>
            <Field className="gap-1.5">
              <FieldLabel>Who can open it</FieldLabel>
              <ToggleGroup
                value={[access]}
                onValueChange={(value) => value[0] && setAccess(value[0] as Tunnel["access_mode"])}
                variant="outline"
                spacing={0}
                className="w-full"
              >
                <ToggleGroupItem value="public" className="h-8 flex-1 text-sm">
                  Anyone with the address
                </ToggleGroupItem>
                <ToggleGroupItem value="private" className="h-8 flex-1 text-sm">
                  Only you
                </ToggleGroupItem>
              </ToggleGroup>
              <p className="text-xs text-muted-foreground">
                Add your own domains, like *.example.com, on the tunnel’s page afterwards.
              </p>
            </Field>
            {create.isError && <p className="text-sm text-destructive">{create.error.message}</p>}
            <DialogFooter>
              <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={create.isPending}>
                Cancel
              </Button>
              <Button type="submit" size="lg" disabled={!ready || create.isPending}>
                {create.isPending ? "Creating…" : "Create tunnel"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** Follows the new tunnel until its machine has connected it (or it fails), then shows the address. */
function Connecting({ id, machineName, onDone }: { id: string; machineName: string; onDone: () => void }) {
  const [startedAt] = useState(() => Date.now())
  const now = useNow(1_000)
  const tunnel = useQuery({
    ...tunnelQuery(id),
    refetchInterval: (query) => (query.state.data?.summary_code === "ready" ? false : 1_000),
  })
  const data = tunnel.data
  const failed = data && (data.summary_code === "expired" || data.desired_state === "deleted")

  if (data && data.summary_code !== "pending" && !failed) {
    return (
      <div className="flex flex-col items-center gap-3 py-4 text-center">
        <CheckCircleIcon weight="fill" className="size-10 text-success" />
        <p className="font-medium">{data.name} is connected</p>
        <CopyRow value={data.stable_endpoint} className="max-w-full font-mono text-code">
          {hostOf(data.stable_endpoint)}
        </CopyRow>
        {data.summary_code === "route_unavailable" && (
          <p className="text-xs text-muted-foreground">
            Nothing answers on its port on {machineName} yet. Start the service and reload.
          </p>
        )}
        <div className="mt-1 flex gap-2">
          <Button variant="secondary" size="lg" onClick={onDone}>
            Done
          </Button>
          <Button size="lg" render={<Link href={`/tunnels/${data.id}`} onClick={onDone} />}>
            Open tunnel
          </Button>
        </div>
      </div>
    )
  }

  if (failed || tunnel.isError) {
    return (
      <div className="flex flex-col items-center gap-3 py-4 text-center">
        <WarningCircleIcon weight="fill" className="size-10 text-destructive" />
        <p className="font-medium">The tunnel didn’t connect</p>
        <p className="text-sm text-muted-foreground">
          {tunnel.isError
            ? tunnel.error.message
            : `${machineName} couldn’t attach it. Check that it’s online and try again.`}
        </p>
        <Button variant="secondary" size="lg" className="mt-1" onClick={onDone}>
          Close
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col items-center gap-3 py-8 text-center" role="status">
      <CircleNotchIcon className="size-6 animate-spin text-muted-foreground" />
      <p className="text-sm text-muted-foreground">
        {now - startedAt > 30_000 ? `Still waiting for ${machineName}…` : `Connecting the tunnel on ${machineName}…`}
      </p>
    </div>
  )
}
