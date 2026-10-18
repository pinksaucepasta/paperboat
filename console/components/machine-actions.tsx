"use client"

import { CoffeeIcon, CopyIcon, PencilSimpleIcon, TerminalIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { api, idempotencyKey, type Machine } from "@/lib/api"
import { useMachineHostnames } from "@/lib/machine-domains"
import { EditMachineDialog } from "@/components/machine-edit"
import { RemoveMachineDialog } from "@/components/machine-remove"
import { RowMenus, type Entry } from "@/components/row-menus"
import { copyWithToast } from "@/lib/clipboard"

/** Keep awake and Allow sleep are one availability setting; this switches between them. */
export function useKeepAwake(machine: Machine) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: (mode: "keep_awake" | "allow_sleep") =>
      api(`machines/${encodeURIComponent(machine.id)}/availability-policy`, {
        method: "PUT",
        headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
        body: JSON.stringify({ mode, expected_version: machine.availability?.desired_version ?? 0 }),
      }),
    onSuccess: (_, mode) => {
      toast.success(mode === "keep_awake" ? `${machine.alias} stays awake` : `${machine.alias} may sleep`)
      return queryClient.invalidateQueries({ queryKey: ["machines"] })
    },
    onError: (error) => toast.error(error.message),
  })
  return {
    isPending: mutation.isPending,
    set: (on: boolean) => mutation.mutate(on ? "keep_awake" : "allow_sleep"),
  }
}

/**
 * A machine's menu: only what its row cannot do itself (opening and tabs live on the row);
 * copying the address and IP is here too, so right-clicking anywhere on the row reaches them. Defined once, shown both as the "…" menu and on
 * right-click, sharing the same dialogs.
 */
function useMachineMenu(machine: Machine) {
  const [dialog, setDialog] = useState<"edit" | "remove" | null>(null)

  const availability = useKeepAwake(machine)

  const copy = (value: string, what: string) => () => void copyWithToast(value, what)
  const [hostname] = useMachineHostnames(machine.alias)

  const entries: Entry[] = [
    { kind: "item", label: "Quick terminal", icon: TerminalIcon, href: `/machines/${machine.id}?terminal=open` },
    { kind: "separator" },
    ...(hostname
      ? [{ kind: "item" as const, label: "Copy address", icon: CopyIcon, onSelect: copy(hostname, "Address") }]
      : []),
    ...(machine.assigned_ip
      ? [{ kind: "item" as const, label: "Copy IP", icon: CopyIcon, onSelect: copy(machine.assigned_ip, "IP") }]
      : []),
    {
      kind: "item",
      label: "Copy SSH command",
      icon: CopyIcon,
      onSelect: copy(`pb ssh ${machine.alias}`, "SSH command"),
    },
  ]
  if (machine.can_manage) {
    entries.push(
      { kind: "separator" },
      { kind: "item", label: "Edit…", icon: PencilSimpleIcon, onSelect: () => setDialog("edit") },
    )
    if (machine.availability) {
      entries.push({
        kind: "check",
        label: "Keep awake",
        icon: CoffeeIcon,
        checked: machine.availability.desired_mode === "keep_awake",
        disabled: availability.isPending,
        onChange: availability.set,
      })
    }
    entries.push(
      { kind: "separator" },
      {
        kind: "item",
        label: "Remove machine",
        icon: TrashIcon,
        destructive: true,
        onSelect: () => setDialog("remove"),
      },
    )
  }

  // Mounted only while open, so each opening starts from the machine's current values.
  const dialogs = (
    <>
      {dialog === "edit" && <EditMachineDialog machine={machine} onClose={() => setDialog(null)} />}
      {dialog === "remove" && <RemoveMachineDialog machine={machine} onClose={() => setDialog(null)} />}
    </>
  )
  return { entries, dialogs }
}

/**
 * Wraps a machine's table row: right-clicking anywhere on it opens the machine's menu, and the
 * row receives the "…" button that opens the same menu.
 */
export function MachineRowMenus({
  machine,
  row,
}: {
  machine: Machine
  row: (actions: React.ReactNode) => React.ReactElement
}) {
  const { entries, dialogs } = useMachineMenu(machine)
  return <RowMenus label={`Actions for ${machine.alias}`} entries={entries} dialogs={dialogs} row={row} />
}
