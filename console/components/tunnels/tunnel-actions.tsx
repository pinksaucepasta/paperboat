"use client"

import { ArrowSquareOutIcon, CopyIcon, PauseIcon, PencilSimpleIcon, PlayIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { deleteTunnel, hostOf, pauseTunnel, resumeTunnel, type Tunnel } from "@/lib/tunnels"
import { ConfirmAction } from "@/components/list-toolbar"
import type { Entry } from "@/components/row-menus"
import { RenameTunnelDialog } from "@/components/tunnels/tunnel-rename"
import { copyWithToast } from "@/lib/clipboard"

/**
 * What can be done to a tunnel, shared by its row menu (and right-click) in the list and its
 * page: open or copy its address, pause or resume it, and delete it.
 */
export function useTunnelActions(tunnel: Tunnel, { onDeleted }: { onDeleted?: () => void } = {}) {
  const queryClient = useQueryClient()
  const [deleting, setDeleting] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const refresh = () =>
    queryClient
      .invalidateQueries({ queryKey: ["tunnels"] })
      .then(() => queryClient.invalidateQueries({ queryKey: ["tunnel", tunnel.id] }))
  const paused = tunnel.desired_state === "paused"
  const toggle = useMutation({
    mutationFn: () => (paused ? resumeTunnel(tunnel) : pauseTunnel(tunnel)),
    onSuccess: async () => {
      await refresh()
      toast.success(paused ? `${tunnel.name} resumed` : `${tunnel.name} paused`)
    },
    onError: (error) =>
      toast.error(paused ? "Couldn’t resume the tunnel" : "Couldn’t pause the tunnel", {
        description: error.message,
      }),
  })

  const entries: Entry[] = [
    {
      kind: "item",
      label: "Open",
      icon: ArrowSquareOutIcon,
      onSelect: () => window.open(tunnel.stable_endpoint, "_blank", "noopener"),
    },
    {
      kind: "item",
      label: "Copy address",
      icon: CopyIcon,
      onSelect: () => void copyWithToast(tunnel.stable_endpoint, "Address"),
    },
    { kind: "separator" },
    { kind: "item", label: "Rename…", icon: PencilSimpleIcon, onSelect: () => setRenaming(true) },
    {
      kind: "item",
      label: paused ? "Resume" : "Pause",
      icon: paused ? PlayIcon : PauseIcon,
      onSelect: () => toggle.mutate(),
    },
    { kind: "separator" },
    { kind: "item", label: "Delete tunnel", icon: TrashIcon, destructive: true, onSelect: () => setDeleting(true) },
  ]

  const dialogs = (
    <>
      {renaming && <RenameTunnelDialog tunnel={tunnel} onClose={() => setRenaming(false)} />}
      <ConfirmAction
        open={deleting}
        onOpenChange={setDeleting}
        title={`Delete ${tunnel.name}?`}
        description={`${hostOf(tunnel.stable_endpoint)} and its public hostnames stop working, and the address is released. DNS records you added for your own domains stay; remove them at your DNS provider.`}
        action="Delete tunnel"
        pending="Deleting…"
        confirmName={tunnel.name}
        mutation={{
          mutationFn: () => deleteTunnel(tunnel),
          onSuccess: async () => {
            await refresh()
            toast.success(`${tunnel.name} deleted`)
            onDeleted?.()
          },
        }}
      />
    </>
  )

  return { entries, dialogs, toggle, paused, openDelete: () => setDeleting(true) }
}
