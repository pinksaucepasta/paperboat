"use client"

import {
  ArrowClockwiseIcon,
  ArrowsLeftRightIcon,
  CopyIcon,
  PauseIcon,
  PlayIcon,
  ShieldCheckIcon,
  TrashIcon,
} from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { copyWithToast } from "@/lib/clipboard"
import {
  drainInstallation,
  refreshNetwork,
  removeInstallation,
  requestUpdate,
  resumeInstallation,
  roleLabel,
  updating,
  verifyIngress,
  type Capability,
  type Node,
} from "@/lib/network"
import { useNow } from "@/lib/use-now"
import { useTimeFormat } from "@/lib/use-time-format"
import { ConfirmAction } from "@/components/list-toolbar"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { errorToast } from "@/components/error-reference"

const updateSteps: Record<string, string> = {
  requested: "waiting for the machine to check in",
  downloading: "downloading",
  verifying: "verifying the signed release",
  swapping: "keeping the current version",
  checking: "installing and checking health",
}

/** The one status a machine shows: pairing, draining, an update in flight or failed, or its readiness. */
function nodeState(
  node: Node,
  now: number,
  time: ReturnType<typeof useTimeFormat>,
): { ok: boolean; label: string; detail?: string } {
  if (node.installations.some((installation) => installation.enrollment_state === "pending"))
    return { ok: false, label: "Finishing pairing" }
  if (node.draining) {
    const deadline = node.installations.find((installation) => installation.drain_deadline)?.drain_deadline
    return {
      ok: false,
      label: "Draining",
      detail: deadline ? `Leaves service by ${time.dateTime(deadline)}` : undefined,
    }
  }
  const running = node.installations.find(updating)?.update
  if (running)
    return { ok: false, label: `Updating to ${running.target_version}: ${updateSteps[running.state] ?? running.state}` }
  // A failed update stays shown for a day: the reason and its support reference are in the tooltip.
  const failed = node.installations
    .map((installation) => installation.update)
    .find(
      (update) =>
        update &&
        (update.state === "failed" || update.state === "rolled_back") &&
        now - Date.parse(update.updated_at) < 86_400_000,
    )
  if (failed)
    return {
      ok: false,
      label: failed.state === "rolled_back" ? "Update rolled back" : "Update failed",
      detail: `${failed.detail ?? ""}${failed.support_reference ? ` Support reference ${failed.support_reference}.` : ""}`,
    }
  const required = node.installations.find((installation) => installation.update_required)
  if (required) return { ok: false, label: "Update required", detail: required.update_required }
  const notReady = node.installations.filter((installation) => !installation.ready)
  if (!notReady.length) return { ok: true, label: "Ready" }
  if (notReady.length === node.installations.length) return { ok: false, label: "Not ready" }
  return { ok: false, label: `${roleLabel(notReady[0].capability)} not ready` }
}

/** The machine's roles: Relay, Edge (capability tunnel), or both. */
const rolesOf = (node: Node) =>
  [...new Set(node.installations.map((installation) => installation.capability))].sort() as Capability[]

/** One machine of a region: name and status dot, roles, version, status, last seen, and its menu. */
export function MachineRow({ node }: { node: Node }) {
  const queryClient = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const [draining, setDraining] = useState<"drain" | "move">()
  const state = nodeState(node, useNow(), useTimeFormat())
  const tunnel = node.installations.find((installation) => installation.capability === "tunnel")
  const available = node.installations.find((installation) => installation.update_available)?.update_available
  const versions = [...new Set(node.installations.map((installation) => installation.version).filter(Boolean))]
  // The newest heartbeat of the machine's components.
  const seen = node.installations
    .map((installation) => installation.last_heartbeat_at)
    .filter(Boolean)
    .sort()
    .at(-1)
  const refresh = () => refreshNetwork(queryClient)
  const update = useMutation({
    mutationFn: () => requestUpdate(node.installations[0]),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} updates at its next check-in`)
    },
    onError: (error) => errorToast("Update could not be requested", error),
  })
  const verify = useMutation({
    mutationFn: () => verifyIngress(tunnel!),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} answers from the internet`)
    },
    onError: (error) => errorToast("Public access check failed", error),
  })
  // Drain and resume act on the machine (its node), so any one of its installations names it.
  const resume = useMutation({
    mutationFn: () => resumeInstallation(node.installations[0]),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} takes work again`)
    },
    onError: (error) => errorToast("Machine could not be resumed", error),
  })
  const entries: Entry[] = [
    { kind: "item", label: "Copy address", icon: CopyIcon, onSelect: () => void copyWithToast(node.host, "Address") },
    ...(tunnel
      ? ([
          {
            kind: "item",
            label: "Check public access",
            icon: ShieldCheckIcon,
            disabled: verify.isPending,
            onSelect: () => verify.mutate(),
          },
        ] satisfies Entry[])
      : []),
    ...(available && !node.installations.some(updating)
      ? ([
          {
            kind: "item",
            label: `Update to ${available}`,
            icon: ArrowClockwiseIcon,
            disabled: update.isPending,
            onSelect: () => update.mutate(),
          },
        ] satisfies Entry[])
      : []),
    { kind: "separator" },
    ...(node.draining
      ? ([
          {
            kind: "item",
            label: "Resume",
            icon: PlayIcon,
            disabled: resume.isPending,
            onSelect: () => resume.mutate(),
          },
        ] satisfies Entry[])
      : []),
    { kind: "item", label: "Drain", icon: PauseIcon, onSelect: () => setDraining("drain") },
    { kind: "item", label: "Drain and move", icon: ArrowsLeftRightIcon, onSelect: () => setDraining("move") },
    { kind: "item", label: "Remove machine", icon: TrashIcon, destructive: true, onSelect: () => setRemoving(true) },
  ]
  return (
    <RowMenus
      label={`Actions for ${node.name}`}
      entries={entries}
      row={(actions) => (
        <tr className="transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            <span className="flex items-center gap-2">
              <span className="truncate font-medium">{node.name}</span>
              <StatusDot online={state.ok} />
            </span>
          </td>
          <td className="hidden md:table-cell">
            <span className="flex items-center gap-1.5">
              {rolesOf(node).map((role) => (
                <Badge key={role} variant="outline" className="font-normal text-muted-foreground">
                  {roleLabel(role)}
                </Badge>
              ))}
            </span>
          </td>
          <td className="hidden truncate font-mono text-code text-muted-foreground sm:table-cell">
            {versions.join(", ")}
          </td>
          <td>
            <span className="flex items-center gap-2 text-muted-foreground">
              <span className="truncate" title={state.detail}>
                {verify.isPending ? "Checking public access…" : state.label}
              </span>
              {!state.detail && available && (
                <span className="hidden truncate text-xs xl:inline">Update available: {available}</span>
              )}
            </span>
          </td>
          <td className="hidden truncate text-muted-foreground lg:table-cell">
            {seen && <Time iso={seen} relative />}
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
      dialogs={
        <>
          <ConfirmAction
            open={draining !== undefined}
            onOpenChange={(open) => !open && setDraining(undefined)}
            title={draining === "move" ? `Drain ${node.name} and move?` : `Drain ${node.name}?`}
            description={
              draining === "move"
                ? "Running work is moved off this machine now. It drains within about 90 seconds."
                : "New work stops going to this machine. Running work finishes (up to 10 minutes)."
            }
            action={draining === "move" ? "Drain and move" : "Drain"}
            pending="Draining…"
            mutation={{
              mutationFn: () => drainInstallation(node.installations[0], draining === "move"),
              onSuccess: async () => {
                await refresh()
                toast.success(`${node.name} is draining`)
              },
            }}
          />
          <ConfirmAction
            open={removing}
            onOpenChange={setRemoving}
            title={`Remove ${node.name}?`}
            description="This machine stops carrying traffic now. To use it again, run the installer and pair it with a new code."
            action="Remove machine"
            pending="Removing…"
            confirmName={node.name}
            mutation={{
              mutationFn: async () => {
                for (const installation of node.installations) await removeInstallation(installation)
              },
              onSuccess: async () => {
                await refresh()
                toast.success(`${node.name} removed`)
              },
            }}
          />
        </>
      }
    />
  )
}
