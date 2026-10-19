"use client"

import { CaretDownIcon, HardDrivesIcon } from "@phosphor-icons/react"
import { machineOwner, edgesQuery, machineLatencyQuery, meQuery, useApiQuery, type Machine } from "@/lib/api"
import { formatPlatform, placeName } from "@/lib/format"
import { MachineAddressDetail } from "@/components/machine-address"
import { Time } from "@/components/time"
import { cn } from "@/lib/utils"
import { CopyRow } from "@/components/copy-button"
import { DangerZone } from "@/components/machine-identity"
import { useCurrentMachine } from "@/components/machine-shell"
import { StatusDot, machineVersion } from "@/components/machine-status"
import { MachineUpdate } from "@/components/machine-update"
import { useKeepAwake } from "@/components/machine-actions"
import { Skeleton } from "@/components/ui/skeleton"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

export function MachineOverview() {
  const machine = useCurrentMachine()
  const { data: me } = useApiQuery(meQuery)
  const owner = machineOwner(machine, me)

  return (
    <div className="flex flex-col gap-10">
      <Section title="Details">
        <dl className="grid border-t border-divider sm:grid-cols-2 sm:gap-x-10">
          <Detail label="Name">{machine.alias}</Detail>
          <Detail label="Owner">
            {owner === "you"
              ? "You"
              : owner === "team"
                ? machine.owner_team_id
                : owner === "shared"
                  ? "Shared with you"
                  : undefined}
          </Detail>
          <Detail label="Address" control>
            <MachineAddressDetail machine={machine} />
          </Detail>
          <Detail label="IP" copy={machine.assigned_ip} mono>
            {machine.assigned_ip}
          </Detail>
          <Detail label="OS">
            {formatPlatform(machine.platform)} {machine.architecture}
          </Detail>
          <Detail label="Version">
            <span className="inline-flex items-center gap-1.5">
              <span className="font-mono text-code">{machineVersion(machine)}</span>
              <MachineUpdate machine={machine} />
              <UpdateHealth machine={machine} />
            </span>
          </Detail>
          <Detail label="Keep awake">{machine.availability && <KeepAwake machine={machine} />}</Detail>
          <Detail label="Workspace" mono>
            {machine.workspace_root}
          </Detail>
          <Detail label="Added">{machine.enrolled_at && <Time iso={machine.enrolled_at} />}</Detail>
          <Detail label="Last seen">
            {machine.online ? "Connected" : machine.last_seen_at && <Time iso={machine.last_seen_at} relative />}
          </Detail>
          <Detail label="Machine ID" mono copy={machine.id}>
            {machine.id}
          </Detail>
          <Detail label="Identity key" mono copy={machine.public_identity_key}>
            {machine.public_identity_key}
          </Detail>
        </dl>
      </Section>
      <Network />
      <DangerZone machine={machine} />
    </div>
  )
}

/** Only says something when updates are not healthy; a healthy machine needs no words here. */
function UpdateHealth({ machine }: { machine: Machine }) {
  const availability = machine.availability
  if (!availability || (availability.update_health === "healthy" && availability.update_rollbacks === 0)) return null
  const rollbacks = availability.update_rollbacks
  return (
    <span className="text-muted-foreground">
      · {availability.update_health.replace(/_/g, " ")}
      {rollbacks > 0 && `, ${rollbacks} rolled back`}
    </span>
  )
}

/** What the machine measured to the edges and relays it can use; empty until it has reported. */
type Relay = { id: string; region: string; self_hosted?: boolean; latency_ms?: number }

/**
 * Where this machine's traffic goes: the account's two edges (previews and tunnels) and the
 * relays used when a direct path is not possible. Times are the machine's own connect times,
 * reported every few minutes, and show a dash until the machine has reported.
 */
function Network() {
  const machine = useCurrentMachine()
  const { data: edges, isPending, isError } = useApiQuery(edgesQuery)
  const { data: measured } = useApiQuery(machineLatencyQuery(machine.id))
  const edgeLatency = new Map((measured ?? []).filter((item) => item.role === "edge").map((item) => [item.node_id, item]))
  const relays: Relay[] = (measured ?? [])
    .filter((item) => item.role === "relay")
    .map((item) => ({ id: item.node_id, region: item.region, self_hosted: item.self_hosted, latency_ms: item.rtt_ms }))

  return (
    <Section title="Network">
      <div className="flex flex-col gap-6">
        <div className="flex flex-col gap-2">
          <h3 className="text-xs text-muted-foreground">Edges</h3>
          <div className="grid gap-3 sm:grid-cols-2">
            {isPending &&
              Array.from({ length: 2 }, (_, index) => <Skeleton key={index} className="h-[5.5rem] rounded-lg" />)}
            {isError && <p className="text-sm text-muted-foreground">Couldn’t load edges</p>}
            {edges?.length === 0 && <p className="text-sm text-muted-foreground">No edges assigned</p>}
            {edges?.map((edge) => (
              <EdgeTile
                key={edge.id}
                region={edge.region}
                status={edge.status}
                latency={edgeLatency.get(edge.id)?.rtt_ms}
                selfHosted={edgeLatency.get(edge.id)?.self_hosted}
              />
            ))}
          </div>
        </div>
        <div className="flex flex-col gap-2">
          <h3 className="text-xs text-muted-foreground">Relays</h3>
          {relays.length > 0 ? (
            <RelayList relays={relays} />
          ) : (
            <p className="text-sm text-muted-foreground">Not reported yet</p>
          )}
        </div>
      </div>
    </Section>
  )
}

function Hosting({ selfHosted, className }: { selfHosted?: boolean; className?: string }) {
  return selfHosted ? (
    // Baseline-aligned like plain text: the icon centers itself and stays out of baseline
    // alignment, so the label's baseline is the text's (an icon's baseline is its bottom edge).
    <span className={cn("inline-flex items-baseline gap-1 text-muted-foreground", className)}>
      <HardDrivesIcon className="size-3.5 self-center" />
      <span>Self-hosted</span>
    </span>
  ) : (
    <span className={cn("text-muted-foreground", className)}>Paperboat</span>
  )
}

function EdgeTile({
  region,
  status,
  latency,
  selfHosted,
}: {
  region: string
  status: string
  latency?: number
  selfHosted?: boolean
}) {
  const ready = status === "ready" || status === "online"
  return (
    <div className="surface-pattern flex flex-col gap-3 p-4">
      {/* Fixed height, so a tile with a status word lines up with one without. */}
      <div className="flex h-[1lh] items-center gap-2">
        <span className="font-medium trim-cap">{placeName(region)}</span>
        <StatusDot online={ready} />
        {!ready && <span className="text-xs text-muted-foreground trim-cap">{status.replace(/_/g, " ")}</span>}
      </div>
      <div className="flex items-baseline justify-between text-sm">
        <Hosting selfHosted={selfHosted} />
        <Latency value={latency} className="text-lg" />
      </div>
    </div>
  )
}

/** Fastest first, in columns, each with a bar scaled to the slowest relay for quick comparison. */
function RelayList({ relays }: { relays: Relay[] }) {
  const sorted = [...relays].sort((a, b) => (a.latency_ms ?? Infinity) - (b.latency_ms ?? Infinity))
  const slowest = Math.max(1, ...sorted.map((relay) => relay.latency_ms ?? 0))
  return (
    <ul className="grid gap-x-8 sm:grid-cols-2 lg:grid-cols-3">
      {sorted.map((relay) => (
        // The latency bar separates rows itself; no divider under it.
        <li key={relay.id} className="flex flex-col gap-2 py-3">
          <div className="flex items-baseline gap-2 text-sm">
            <span className="truncate">{placeName(relay.region)}</span>
            {relay.self_hosted && <Hosting selfHosted className="text-xs" />}
            <Latency value={relay.latency_ms} className="ml-auto" />
          </div>
          <div className="h-0.5 overflow-hidden rounded-full bg-foreground/8">
            {relay.latency_ms !== undefined && (
              <div
                className="h-full rounded-full bg-foreground/40"
                style={{ width: `${(relay.latency_ms / slowest) * 100}%` }}
              />
            )}
          </div>
        </li>
      ))}
    </ul>
  )
}

function Latency({ value, className }: { value?: number; className?: string }) {
  return (
    <span
      className={cn("font-mono text-code text-muted-foreground", value !== undefined && "text-foreground", className)}
    >
      {value !== undefined ? `${value} ms` : "—"}
    </span>
  )
}

/**
 * Regions look like "in-mumbai", "fi-helsinki-1" or "mumbai": an optional two-letter country
 * prefix and an optional index. People read them as the city.
 */

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <h2 className="text-sm font-medium">{title}</h2>
      {children}
    </section>
  )
}

/**
 * Reads like the other details ("On"); those who manage the machine change it from the same
 * caret menu the address uses. Until the machine confirms a change, a muted note says so.
 */
function KeepAwake({ machine }: { machine: Machine }) {
  const keepAwake = useKeepAwake(machine)
  const availability = machine.availability!
  const on = availability.desired_mode === "keep_awake"
  const note =
    availability.status === "unsupported"
      ? "Not supported on this machine"
      : availability.status === "error"
        ? "Couldn’t apply"
        : availability.status === "pending"
          ? `Waiting for ${machine.alias}`
          : undefined
  const value = on ? "On" : "Off"
  return (
    <span className="flex items-center gap-2">
      {machine.can_manage && availability.status !== "unsupported" ? (
        <DropdownMenu>
          <DropdownMenuTrigger
            disabled={keepAwake.isPending}
            className="inline-flex items-center gap-1 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
          >
            {value}
            <CaretDownIcon className="size-3.5 text-muted-foreground" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-56">
            <DropdownMenuRadioGroup value={on ? "on" : "off"} onValueChange={(next) => keepAwake.set(next === "on")}>
              <DropdownMenuRadioItem value="on">On, stays awake</DropdownMenuRadioItem>
              <DropdownMenuRadioItem value="off">Off, may sleep</DropdownMenuRadioItem>
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      ) : (
        <span>{value}</span>
      )}
      {note && (
        <span className={cn("text-muted-foreground", availability.status === "error" && "text-destructive")}>
          · {note}
        </span>
      )}
    </span>
  )
}

export function Detail({
  label,
  mono,
  copy,
  control,
  children,
}: {
  label: string
  mono?: boolean
  copy?: string
  /** The value is its own full-width control (like the copy rows), so it is not clipped. */
  control?: boolean
  children: React.ReactNode
}) {
  const value = children || <span className="text-muted-foreground">—</span>
  return (
    <div className="flex min-h-11 min-w-0 items-center gap-4 border-b border-divider py-1 text-sm">
      <dt className="w-28 shrink-0 text-muted-foreground">{label}</dt>
      <dd className={cn("min-w-0 flex-1", mono && "font-mono text-code")}>
        {copy ? (
          <CopyRow value={copy} className="-ml-2.5">
            {value}
          </CopyRow>
        ) : control ? (
          value
        ) : (
          <div className="truncate">{value}</div>
        )}
      </dd>
    </div>
  )
}
