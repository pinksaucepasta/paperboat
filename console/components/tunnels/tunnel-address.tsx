"use client"

import { toast } from "sonner"
import { CheckIcon, PencilSimpleIcon, WarningIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { hostOf, patchTunnel, subdomainAvailabilityQuery, subdomainPattern, type Tunnel } from "@/lib/tunnels"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ErrorMessage } from "@/components/error-reference"

/** The tunnel's address under its base domain: "calm-summer-beach-3476.example.test" → label and ".example.test". */
const split = (tunnel: Tunnel) => {
  const host = hostOf(tunnel.stable_endpoint)
  const dot = Math.max(host.indexOf("."), 0)
  return { label: host.slice(0, dot), base: host.slice(dot) }
}

/** The pencil next to a tunnel's address; opens the dialog that picks a new one. */
export function ChangeAddress({ tunnel, disabled }: { tunnel: Tunnel; disabled?: boolean }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label="Change address"
        disabled={disabled}
        onClick={() => setOpen(true)}
        className="text-muted-foreground"
      >
        <PencilSimpleIcon className="size-4" />
      </Button>
      {open && <ChangeAddressDialog tunnel={tunnel} onClose={() => setOpen(false)} />}
    </>
  )
}

/** The typed label once the typing has paused, so the availability check runs per name, not per keystroke. */
function useSettled(value: string) {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), 400)
    return () => clearTimeout(timer)
  }, [value])
  return settled
}

function ChangeAddressDialog({ tunnel, onClose }: { tunnel: Tunnel; onClose: () => void }) {
  const queryClient = useQueryClient()
  const { label: current, base } = split(tunnel)
  const [typed, setTyped] = useState(current)
  const label = typed.trim()
  const settled = useSettled(label)
  const unchanged = label === current
  const wellFormed = subdomainPattern.test(label) && !/^\d+$/.test(label) && !label.startsWith("xn--")
  const check = useQuery({
    ...subdomainAvailabilityQuery(tunnel.id, settled),
    enabled: settled === label && !unchanged && wellFormed,
  })
  const save = useMutation({
    mutationFn: () => patchTunnel(tunnel, { subdomain: label }),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: ["tunnels"] })
      await queryClient.invalidateQueries({ queryKey: ["tunnel", tunnel.id] })
      // A move nothing serves finishes while the request answers; a running one is followed on the tunnel page.
      const moved = (result as { stable_endpoint?: string } | undefined)?.stable_endpoint
      if (moved && hostOf(moved) === `${label}${base}`) toast.success("Address changed", { description: `The tunnel is now at ${hostOf(moved)}.` })
      onClose()
    },
  })
  const available = check.data?.available === true && settled === label
  const problem = unchanged
    ? undefined
    : !label
      ? "Enter a name."
      : !wellFormed
        ? "3 to 63 lowercase letters, numbers and dashes, not starting or ending with a dash, and not only numbers."
        : settled === label && check.data && !check.data.available
          ? check.data.message
          : undefined
  return (
    <Dialog open onOpenChange={(next) => !next && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Change address</DialogTitle>
          <DialogDescription>
            Pick the name this tunnel answers on. The new address starts working first; the current one keeps
            working until the new one is ready everywhere, then stops. Anyone using it, including webhooks and
            bookmarks, must switch to the new one.
          </DialogDescription>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (available) save.mutate()
          }}
        >
          <Field data-invalid={Boolean(problem)} className="gap-1.5">
            <FieldLabel htmlFor="tunnel-address">Address</FieldLabel>
            <div className="flex items-center gap-1 font-mono text-code">
              <Input
                id="tunnel-address"
                autoFocus
                value={typed}
                onChange={(event) => {
                  save.reset()
                  setTyped(event.target.value.toLowerCase())
                }}
                aria-invalid={Boolean(problem)}
                autoComplete="off"
                spellCheck={false}
                className="min-w-0 flex-1 font-mono"
              />
              <span className="shrink-0 text-muted-foreground">{base}</span>
            </div>
            <p className="flex min-h-5 items-center gap-1.5 text-xs" role="status">
              {problem ? (
                <span className="flex items-center gap-1.5 text-destructive">
                  <WarningIcon className="size-3.5" />
                  {problem}
                </span>
              ) : available ? (
                <span className="flex items-center gap-1.5 text-muted-foreground">
                  <CheckIcon className="size-3.5" />
                  Available
                </span>
              ) : check.isError ? (
                <span className="flex items-center gap-1.5 text-destructive">
                  <WarningIcon className="size-3.5" />
                  <ErrorMessage error={check.error} />
                  <Button type="button" variant="link" size="sm" className="h-auto p-0" onClick={() => check.refetch()}>
                    Try again
                  </Button>
                </span>
              ) : !unchanged && wellFormed ? (
                <span className="text-muted-foreground">Checking…</span>
              ) : null}
            </p>
          </Field>
          {save.isError && (
            <p className="text-sm text-destructive">
              <ErrorMessage error={save.error} />
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!available || save.isPending}>
              {save.isPending ? "Saving…" : "Change address"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
