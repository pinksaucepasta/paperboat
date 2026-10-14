"use client"

import { ArrowsClockwiseIcon, WarningCircleIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useApiQuery, type Machine } from "@/lib/api"
import {
  configStatusQuery,
  directionLabels,
  humanize,
  syncState,
  type ConfigEnvironment,
  type ConfigPath,
} from "@/lib/config-sync"
import { formatBytes } from "@/lib/format"
import { useListScope } from "@/components/list-toolbar"
import { Detail } from "@/components/machine-overview"
import { useCurrentMachine } from "@/components/machine-shell"
import { StatusDot } from "@/components/machine-status"
import { Section } from "@/components/section"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

/** The machine page's Config sync tab. */
export function MachineConfigSync() {
  return <ConfigSync machine={useCurrentMachine()} />
}

/**
 * Which machines sync config files with a Git repository, and how that's going. Read-only:
 * conflicts are reported here and resolved on the machine.
 */
export function ConfigSync({ machine: tabMachine }: { machine?: Machine }) {
  const { scope, machine } = useListScope(tabMachine)
  const status = useApiQuery(configStatusQuery)
  const environments = status.data?.environments ?? []

  if (status.isPending) {
    return (
      <div className="flex flex-col gap-3">
        <Skeleton className="h-5 w-1/3" />
        <Skeleton className="h-5 w-1/2" />
      </div>
    )
  }
  if (status.isError) {
    return (
      <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
        Couldn’t load config sync. {status.error.message}
        <Button variant="outline" size="sm" onClick={() => status.refetch()}>
          Retry
        </Button>
      </div>
    )
  }

  if (scope) {
    const environment = environments.find((entry) => entry.machine_id === scope)
    return environment ? (
      <MachineSync environment={environment} />
    ) : (
      <NotSetUp name={machine?.alias ?? "This machine"} />
    )
  }
  if (!environments.length) return <NotSetUp />
  return <AllMachines environments={environments} />
}

function NotSetUp({ name }: { name?: string }) {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ArrowsClockwiseIcon />
        </EmptyMedia>
        <EmptyTitle>{name ? `${name} doesn’t sync config` : "No machine syncs config yet"}</EmptyTitle>
        <EmptyDescription>
          Keep files like your shell and editor settings in a Git repository, the same on every machine. Set it up on
          the machine with pb config sync init.
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

function AllMachines({ environments }: { environments: ConfigEnvironment[] }) {
  const conflicted = environments.filter((entry) => entry.conflicts?.length)
  const sorted = [...environments].sort(
    (a, b) => Number(syncState(b).attention) - Number(syncState(a).attention) || a.alias.localeCompare(b.alias),
  )
  return (
    <section className="flex flex-col gap-4">
      {conflicted.length > 0 && (
        <div className="flex items-start gap-3 rounded-lg bg-surface p-4 text-sm">
          <WarningCircleIcon weight="fill" className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div className="flex min-w-0 flex-col gap-0.5">
            <p className="font-medium">
              {conflicted.length === 1
                ? `${conflicted[0].conflicts!.length === 1 ? "A file" : "Files"} on ${conflicted[0].alias} changed in two places`
                : `Files on ${conflicted.length} machines changed in two places`}
            </p>
            <p className="text-muted-foreground">
              Those files stop syncing until you choose which version to keep, on the machine with pb config conflict.
            </p>
          </div>
        </div>
      )}
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-[20%]" />
          <col className="hidden md:table-column" />
          <col className="hidden w-[18%] lg:table-column" />
          <col className="hidden w-[8%] sm:table-column" />
          <col className="w-[22%]" />
          <col className="hidden w-[14%] sm:table-column" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Machine</th>
            <th className="hidden md:table-cell">Repository</th>
            <th className="hidden lg:table-cell">Direction</th>
            <th className="hidden text-right sm:table-cell">Files</th>
            <th>Status</th>
            <th className="hidden sm:table-cell">Last synced</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {sorted.map((environment) => {
            const state = syncState(environment)
            return (
              <tr
                key={environment.environment_id}
                className="group relative transition-colors hover:bg-surface [&>td]:h-14 [&>td]:px-3"
              >
                <td>
                  <Link
                    href={`/machines/${encodeURIComponent(environment.machine_id)}/config`}
                    className="block truncate-x font-medium trim-cap after:absolute after:inset-0"
                  >
                    {environment.alias}
                  </Link>
                </td>
                <td className="hidden truncate text-muted-foreground md:table-cell">
                  {environment.repository_name ?? "—"}
                </td>
                <td className="hidden truncate text-muted-foreground lg:table-cell">
                  {directionLabels[environment.mode ?? ""] ?? "—"}
                </td>
                <td className="hidden text-right text-muted-foreground tabular-nums sm:table-cell">
                  {environment.managed_path_count}
                </td>
                <td>
                  <span
                    className={`flex items-center gap-2 ${state.danger ? "text-destructive" : "text-muted-foreground"}`}
                  >
                    <StatusDot online={state.ok} />
                    <span className="truncate">{state.label}</span>
                  </span>
                </td>
                <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
                  {environment.last_successful_sync_at ? (
                    <span className="relative z-10">
                      <Time iso={environment.last_successful_sync_at} relative />
                    </span>
                  ) : (
                    "—"
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </section>
  )
}

function MachineSync({ environment }: { environment: ConfigEnvironment }) {
  const state = syncState(environment)
  const conflicts = environment.conflicts ?? []
  const review = environment.review ?? []
  const skipped = environment.skipped ?? []
  const actions = environment.recovery_actions ?? []
  return (
    <div className="flex flex-col gap-10">
      <div className="flex flex-col gap-1.5">
        <span
          className={`flex items-center gap-2 text-sm ${state.danger ? "text-destructive" : "text-muted-foreground"}`}
        >
          <StatusDot online={state.ok} />
          {state.label}
          {environment.last_successful_sync_at && (
            <span className="text-muted-foreground">
              · last synced <Time iso={environment.last_successful_sync_at} relative />
            </span>
          )}
        </span>
        {actions.length > 0 && (
          <p className="text-sm text-muted-foreground">To fix: {actions.map(humanize).join("; ")}.</p>
        )}
      </div>

      <section className="flex flex-col gap-3">
        <h2 className="text-sm font-medium">Details</h2>
        <dl className="grid border-t border-divider sm:grid-cols-2 sm:gap-x-10">
          <Detail label="Repository">{environment.repository_name}</Detail>
          <Detail label="Direction">{directionLabels[environment.mode ?? ""]}</Detail>
          <Detail label="Files synced">{String(environment.managed_path_count)}</Detail>
          <Detail label="Updates">{environment.automatic_updates ? "Automatic" : "After approval"}</Detail>
          <Detail label="Last synced">
            {environment.last_successful_sync_at && <Time iso={environment.last_successful_sync_at} />}
          </Detail>
          <Detail label="Last attempt">
            {environment.last_attempt_at && <Time iso={environment.last_attempt_at} relative />}
          </Detail>
        </dl>
      </section>

      {conflicts.length > 0 && (
        <PathList
          title="Conflicts"
          paths={conflicts}
          note="Changed both here and in the repository. They stop syncing until you choose which version to keep, on the machine with pb config conflict."
        />
      )}
      {review.length > 0 && (
        <PathList
          title="Waiting for approval"
          paths={review}
          note="Changes from the repository that apply once approved on the machine with pb config approve."
        />
      )}
      {skipped.length > 0 && <PathList title="Skipped" paths={skipped} note="Not synced, for the reason shown." />}
    </div>
  )
}

function PathList({ title, paths, note }: { title: string; paths: ConfigPath[]; note: string }) {
  return (
    <Section title={title}>
      <p className="border-b border-divider py-2 text-sm text-muted-foreground">{note}</p>
      {paths.map((path) => (
        <div key={path.path} className="flex min-h-11 items-center gap-4 border-b border-divider py-1 text-sm">
          <span className="min-w-0 flex-1 truncate font-mono text-code">{path.path}</span>
          {path.bytes !== undefined && (
            <span className="hidden shrink-0 text-muted-foreground tabular-nums sm:inline">
              {formatBytes(path.bytes)}
            </span>
          )}
          <span className="shrink-0 text-muted-foreground">{humanize(path.reason)}</span>
        </div>
      ))}
    </Section>
  )
}
