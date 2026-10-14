"use client"

import { CheckIcon, CircleNotchIcon, PencilSimpleIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { patchTunnel, type Tunnel } from "@/lib/tunnels"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/** The server's rule (tunnelv1 validateTunnelName): 1–63 of letters, digits, ".", "_", "-", not starting with punctuation. */
const namePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/

function nameProblem(name: string) {
  if (!name) return "Enter a name."
  if (!namePattern.test(name))
    return "Letters, numbers, dots, dashes and underscores. Up to 63, starting with a letter or number."
}

/** One save and validation for renaming a tunnel, from its page or from the list. The address stays. */
function useRename(tunnel: Tunnel, onDone: () => void) {
  const queryClient = useQueryClient()
  const [name, setName] = useState(tunnel.name)
  const final = name.trim()
  const problem = final === tunnel.name ? undefined : nameProblem(final)
  const save = useMutation({
    mutationFn: () => patchTunnel(tunnel, { name: final }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["tunnels"] })
      await queryClient.invalidateQueries({ queryKey: ["tunnel", tunnel.id] })
      onDone()
    },
  })
  const submit = () => {
    if (final === tunnel.name) return onDone()
    if (!problem) save.mutate()
  }
  return { name, setName, problem: problem ?? (save.isError ? save.error.message : undefined), save, submit }
}

const inPlace =
  "h-auto rounded-none border-0 border-b border-dashed border-border bg-transparent px-0 shadow-none focus-visible:border-ring focus-visible:ring-0 dark:bg-transparent"

/** The tunnel's name on its page, edited in place (DESIGN.md: hovering shows a pencil). */
export function TunnelTitle({ tunnel }: { tunnel: Tunnel }) {
  const [editing, setEditing] = useState(false)
  if (editing) return <TitleEditor tunnel={tunnel} onDone={() => setEditing(false)} />
  return (
    <div className="group/title flex min-w-0 items-center gap-1">
      <h1 className="truncate font-heading text-2xl font-semibold tracking-tight">{tunnel.name}</h1>
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label="Rename"
        onClick={() => setEditing(true)}
        className="text-muted-foreground opacity-0 transition-opacity group-hover/title:opacity-100 focus-visible:opacity-100"
      >
        <PencilSimpleIcon className="size-4" />
      </Button>
    </div>
  )
}

function TitleEditor({ tunnel, onDone }: { tunnel: Tunnel; onDone: () => void }) {
  const edit = useRename(tunnel, onDone)
  return (
    <form
      className="flex min-w-0 flex-col gap-1"
      onSubmit={(event) => {
        event.preventDefault()
        edit.submit()
      }}
      onKeyDown={(event) => event.key === "Escape" && !edit.save.isPending && onDone()}
    >
      <div className="flex min-w-0 items-center gap-1">
        <Input
          autoFocus
          aria-label="Name"
          value={edit.name}
          onChange={(event) => edit.setName(event.target.value)}
          aria-invalid={Boolean(edit.problem)}
          size={Math.max(edit.name.length, 4)}
          className={`${inPlace} w-auto py-0 font-heading text-2xl font-semibold tracking-tight md:text-2xl`}
          autoComplete="off"
          spellCheck={false}
        />
        <Button type="submit" variant="ghost" size="icon-sm" aria-label="Save" disabled={edit.save.isPending}>
          {edit.save.isPending ? <CircleNotchIcon className="size-4 animate-spin" /> : <CheckIcon className="size-4" />}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label="Cancel"
          onClick={onDone}
          disabled={edit.save.isPending}
        >
          <XIcon className="size-4" />
        </Button>
      </div>
      {edit.problem && <p className="text-xs text-destructive">{edit.problem}</p>}
    </form>
  )
}

/** "Rename…" from the list's row menu: the same edit as a dialog. */
export function RenameTunnelDialog({ tunnel, onClose }: { tunnel: Tunnel; onClose: () => void }) {
  const edit = useRename(tunnel, onClose)
  return (
    <Dialog open onOpenChange={(next) => !next && !edit.save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Rename {tunnel.name}</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            edit.submit()
          }}
        >
          <Field data-invalid={Boolean(edit.problem)} className="gap-1.5">
            <FieldLabel htmlFor="tunnel-rename">Name</FieldLabel>
            <Input
              id="tunnel-rename"
              autoFocus
              value={edit.name}
              onChange={(event) => edit.setName(event.target.value)}
              aria-invalid={Boolean(edit.problem)}
              autoComplete="off"
              spellCheck={false}
            />
            {edit.problem ? (
              <FieldError>{edit.problem}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">The address stays the same.</p>
            )}
          </Field>
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={edit.save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={edit.save.isPending}>
              {edit.save.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
