"use client"

import {
  ArrowSquareOutIcon,
  CheckCircleIcon,
  CircleNotchIcon,
  PlusIcon,
  WarningCircleIcon,
} from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useMachines, useWorkspace, type Machine } from "@/lib/api"
import { createPreview, hostOf, previewQuery, type Preview } from "@/lib/tunnels"
import { useNow } from "@/lib/use-now"
import { CopyRow } from "@/components/copy-button"
import { HostnameInput } from "@/components/domains/hostname-input"
import { canPreview, MachineChoice, usePort } from "@/components/tunnels/machine-choice"
import { isLimitReached, LimitReached } from "@/components/limit-reached"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

const lifetimes = [
  { hours: 1, label: "1 hour" },
  { hours: 8, label: "8 hours" },
  { hours: 24, label: "24 hours" },
]

export function NewPreviewButton({ machine }: { machine?: Machine }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size="lg" onClick={() => setOpen(true)}>
        <PlusIcon data-icon="inline-start" />
        New preview
      </Button>
      {/* Mounted only while open, so each opening starts from a clean form. */}
      {open && <NewPreviewDialog machine={machine} onClose={() => setOpen(false)} />}
    </>
  )
}

/**
 * Pick a machine and a port; the preview starts on that machine and the dialog stays open until
 * its address answers, then hands you the address.
 */
function NewPreviewDialog({ machine, onClose }: { machine?: Machine; onClose: () => void }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const machines = useMachines()
  const [chosen, setChosen] = useState<Machine | undefined>(machine)
  const target = chosen ?? machines.data?.find(canPreview)
  const port = usePort()
  const [access, setAccess] = useState<Preview["access_mode"]>("public")
  const [hours, setHours] = useState(1)
  // "Generated" (a random address) or a name on one of your domains.
  const [ownAddress, setOwnAddress] = useState(false)
  const [hostname, setHostname] = useState("")
  const hostnameProblem =
    ownAddress && hostname && !/^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/.test(hostname)
      ? "Enter an address like demo.acme.dev."
      : undefined

  const create = useMutation({
    mutationFn: () =>
      createPreview({
        machineId: target!.id,
        workspace: workspace?.id ?? "personal",
        port: port.value,
        access,
        hours,
        hostname: ownAddress ? hostname : undefined,
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["previews"] }),
  })
  const ready = Boolean(target && canPreview(target) && port.value && (!ownAddress || (hostname && !hostnameProblem)))

  return (
    <Dialog open onOpenChange={(next) => !next && !create.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New preview</DialogTitle>
        </DialogHeader>
        {create.data ? (
          <Starting id={create.data} machineName={target?.alias ?? "the machine"} port={port.value} onDone={onClose} />
        ) : (
          <form
            className="flex min-w-0 flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (ready) create.mutate()
            }}
          >
            <div className="grid grid-cols-[minmax(0,1fr)_6rem] gap-3">
              <Field className="min-w-0 gap-1.5">
                <FieldLabel>Machine</FieldLabel>
                <MachineChoice value={target} onChange={setChosen} usable={canPreview} />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor="preview-port">Port</FieldLabel>
                <Input
                  id="preview-port"
                  autoFocus
                  inputMode="numeric"
                  value={port.text}
                  onChange={(event) => port.set(event.target.value)}
                  aria-invalid={port.text !== "" && !port.value}
                  className="font-mono"
                />
              </Field>
            </div>
            <Field className="min-w-0 gap-1.5">
              <FieldLabel>Address</FieldLabel>
              <ToggleGroup
                value={[ownAddress ? "own" : "generated"]}
                onValueChange={(value) => value[0] && setOwnAddress(value[0] === "own")}
                variant="outline"
                spacing={0}
                className="w-full"
              >
                <ToggleGroupItem value="generated" className="h-8 flex-1 text-sm">
                  Generated
                </ToggleGroupItem>
                <ToggleGroupItem value="own" className="h-8 flex-1 text-sm">
                  On your domain
                </ToggleGroupItem>
              </ToggleGroup>
              {ownAddress && (
                <>
                  <HostnameInput
                    value={hostname}
                    onChange={setHostname}
                    invalid={Boolean(hostnameProblem)}
                    placeholder="demo"
                  />
                  {hostnameProblem && <FieldError>{hostnameProblem}</FieldError>}
                </>
              )}
            </Field>
            <Field className="gap-1.5">
              <FieldLabel>Who can open it</FieldLabel>
              <ToggleGroup
                value={[access]}
                onValueChange={(value) => value[0] && setAccess(value[0] as Preview["access_mode"])}
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
            </Field>
            <Field className="gap-1.5">
              <FieldLabel>Ends after</FieldLabel>
              <ToggleGroup
                value={[String(hours)]}
                onValueChange={(value) => value[0] && setHours(Number(value[0]))}
                variant="outline"
                spacing={0}
                className="w-full"
              >
                {lifetimes.map((entry) => (
                  <ToggleGroupItem key={entry.hours} value={String(entry.hours)} className="h-8 flex-1 text-sm">
                    {entry.label}
                  </ToggleGroupItem>
                ))}
              </ToggleGroup>
              <p className="text-xs text-muted-foreground">You can stop it any time before.</p>
            </Field>
            {isLimitReached(create.error) ? (
              <LimitReached error={create.error} />
            ) : create.isError && <p className="text-sm text-destructive">{create.error.message}</p>}
            <DialogFooter>
              <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={create.isPending}>
                Cancel
              </Button>
              <Button type="submit" size="lg" disabled={!ready || create.isPending}>
                {create.isPending ? "Creating…" : "Create preview"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** Follows a new preview until its address answers (or it fails), then shows the address. */
export function Starting({
  id,
  machineName,
  port,
  note,
  label = "Preview",
  onDone,
}: {
  id: string
  machineName: string
  /** The port it forwards to; absent for a shared file, which the machine serves itself. */
  port?: number
  /** A line under the address, such as how a shared file opens. */
  note?: string
  /** What went live, for the heading ("report.html is live"); a plain preview otherwise. */
  label?: string
  onDone: () => void
}) {
  // eslint-disable-next-line no-restricted-syntax -- this dialog opens on a click, never in a server render
  const startedAt = useState(() => Date.now())[0]
  const now = useNow(1_000)
  const preview = useQuery({
    ...previewQuery(id),
    refetchInterval: (query) => (query.state.data?.state === "ready" ? false : 1_000),
  })
  const data = preview.data
  const failed = data && (data.state === "expired" || data.state === "stopped" || data.allocation_state === "failed")

  if (data?.state === "ready") {
    return (
      <div className="flex flex-col items-center gap-3 py-4 text-center">
        <CheckCircleIcon weight="fill" className="size-10 text-success" />
        <p className="font-medium">{label} is live</p>
        <CopyRow value={data.endpoint} className="max-w-full font-mono text-code">
          {hostOf(data.endpoint)}
        </CopyRow>
        {note && <p className="text-xs text-muted-foreground">{note}</p>}
        {port !== undefined && data.origin_state === "unavailable" && (
          <p className="text-xs text-muted-foreground">
            Nothing answers on port {port} on {machineName} yet. Start your app and reload.
          </p>
        )}
        <div className="mt-1 flex gap-2">
          <Button variant="secondary" size="lg" onClick={onDone}>
            Done
          </Button>
          <Button size="lg" render={<a href={data.endpoint} target="_blank" rel="noopener" />} nativeButton={false}>
            <ArrowSquareOutIcon data-icon="inline-start" />
            Open
          </Button>
        </div>
      </div>
    )
  }

  if (failed || preview.isError) {
    return (
      <div className="flex flex-col items-center gap-3 py-4 text-center">
        <WarningCircleIcon weight="fill" className="size-10 text-destructive" />
        <p className="font-medium">The preview didn’t start</p>
        <p className="text-sm text-muted-foreground">
          {preview.isError
            ? preview.error.message
            : `${machineName} couldn’t connect it. Check that it’s online and try again.`}
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
        {now - startedAt > 30_000 ? `Still waiting for ${machineName}…` : `Starting the preview on ${machineName}…`}
      </p>
    </div>
  )
}
