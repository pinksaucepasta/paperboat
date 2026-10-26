"use client"

import { PlusIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { useWorkspace, type Machine } from "@/lib/api"
import { usePort } from "@/components/tunnels/machine-choice"
import { CommandBlock } from "@/components/add-machine-dialog"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
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
 * A tunnel is created on its machine: its Paperboat service holds the tunnel's runtime connection and proves the
 * machine's identity, which the control plane requires. The dialog builds the exact command to run
 * there; the tunnel then appears in the list. Hostnames and your own domains are added on its page.
 */
function NewTunnelDialog({ machine, onClose }: { machine?: Machine; onClose: () => void }) {
  const workspace = useWorkspace()
  const [name, setName] = useState("")
  const port = usePort()
  const [access, setAccess] = useState<"public" | "private">("public")
  const nameValue = name.replace(/-+$/, "") || "my-app"
  const command = [
    `pb tunnel create ${nameValue} --port ${port.value || "3000"}`,
    access === "private" && "--private",
    workspace?.kind === "team" && `--workspace ${workspace.id}`,
  ]
    .filter(Boolean)
    .join(" ")

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New tunnel</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-2">
          <CommandBlock command={command} />
          <p className="text-xs text-muted-foreground">
            Run it on {machine ? machine.alias : "the machine"} that serves the port. The tunnel shows up here when it
            connects.
          </p>
        </div>
        <div className="flex flex-col gap-4 border-t border-divider pt-4">
          <div className="grid grid-cols-[minmax(0,1fr)_6rem] gap-3">
            <Field className="min-w-0 gap-1.5">
              <FieldLabel htmlFor="tunnel-name">Name</FieldLabel>
              <Input
                id="tunnel-name"
                autoFocus
                placeholder="my-app"
                value={name}
                onChange={(event) => setName(slug(event.target.value))}
                autoComplete="off"
                spellCheck={false}
              />
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
              onValueChange={(value) => value[0] && setAccess(value[0] as typeof access)}
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
        </div>
      </DialogContent>
    </Dialog>
  )
}
