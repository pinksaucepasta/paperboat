"use client"

import {
  ArrowsClockwiseIcon,
  CopyIcon,
  KeyIcon,
  LockSimpleIcon,
  LockSimpleOpenIcon,
  PlusIcon,
  TrashIcon,
  UploadSimpleIcon,
} from "@phosphor-icons/react"
import { useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { copyWithToast } from "@/lib/clipboard"
import {
  globalScopeQuery,
  lockVault,
  machineScopeQuery,
  saveVariables,
  useVault,
  valueKey,
  type ScopeView,
  type Variable,
} from "@/lib/environment"
import { ImportDialog, UnlockDialog, VariableDialog, type Target } from "@/components/environment/env-dialogs"
import { ConfirmAction, SearchField, useListScope } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

/** The machine page's Environment tab. */
export function MachineEnvironment() {
  return <EnvironmentList machine={useCurrentMachine()} />
}

/** One row: a variable as the selected view sees it. On a machine, `overrides` marks a machine value hiding a global. */
type Row = { variable: Variable; overrides: boolean }

/**
 * Environment variables, end-to-end encrypted. "All machines" lists the shared values; a machine
 * (picked in the header, or its own tab) lists what it actually gets: shared values plus its
 * overrides, and whether it has picked up the latest. Names are always visible; values only once
 * the vault is unlocked in this tab.
 */
export function EnvironmentList({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const ws = workspace?.id ?? ""
  const { scope, machine } = useListScope(tabMachine)
  const vault = useVault()
  const global = useApiQuery({ ...globalScopeQuery(ws), enabled: Boolean(workspace) })
  const local = useApiQuery({ ...machineScopeQuery(ws, scope ?? ""), enabled: Boolean(workspace && scope) })
  const [search, setSearch] = useState("")
  const text = useDeferredValue(search.trim().toLowerCase())
  const [dialog, setDialog] = useState<"unlock" | "add" | "import" | null>(null)
  // What to do once the vault opens (adding or importing needs it).
  const [afterUnlock, setAfterUnlock] = useState<"add" | "import" | null>(null)
  const needUnlocked = (next: "add" | "import") => {
    if (vault.unlocked) setDialog(next)
    else {
      setAfterUnlock(next)
      setDialog("unlock")
    }
  }

  const machineNames = new Set(local.data?.variables.map((variable) => variable.name))
  const rows: Row[] = scope
    ? [
        ...(local.data?.variables ?? []).map((variable) => ({
          variable,
          overrides: Boolean(global.data?.variables.some((shared) => shared.name === variable.name)),
        })),
        ...(global.data?.variables ?? [])
          .filter((variable) => !machineNames.has(variable.name))
          .map((variable) => ({ variable, overrides: false })),
      ]
    : (global.data?.variables ?? []).map((variable) => ({ variable, overrides: false }))
  const shown = rows
    .filter((row) => !text || row.variable.name.toLowerCase().includes(text))
    .sort((a, b) => a.variable.name.localeCompare(b.variable.name))
  const loading = global.isPending || (Boolean(scope) && local.isPending)
  const failed = global.isError ? global.error : local.isError ? local.error : null
  const defaultTarget: Target = scope ? { scope: "machine", machineId: scope } : { scope: "global" }
  const columns = 4 + (scope ? 1 : 0)

  const dialogs = (
    <>
      {dialog === "unlock" && (
        <UnlockDialog
          onClose={() => setDialog(null)}
          onUnlocked={() => {
            setDialog(afterUnlock)
            setAfterUnlock(null)
          }}
        />
      )}
      {dialog === "add" && <VariableDialog target={defaultTarget} onClose={() => setDialog(null)} />}
      {dialog === "import" && <ImportDialog target={defaultTarget} onClose={() => setDialog(null)} />}
    </>
  )

  const actions = (
    <>
      <Button variant="secondary" size="lg" onClick={() => needUnlocked("import")}>
        <UploadSimpleIcon data-icon="inline-start" />
        Import .env
      </Button>
      <Button size="lg" onClick={() => needUnlocked("add")}>
        <PlusIcon data-icon="inline-start" />
        Add variable
      </Button>
    </>
  )

  if (!loading && !failed && rows.length === 0) {
    return (
      <Empty className="py-16">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <KeyIcon />
          </EmptyMedia>
          <EmptyTitle>No variables yet</EmptyTitle>
          <EmptyDescription>
            {machine
              ? `Add variables just for ${machine.alias}, or shared ones under All machines. They’re encrypted end to end.`
              : "Add variables every machine gets, like API keys. They’re encrypted end to end; only your machines can read them."}
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent className="flex-row justify-center">{actions}</EmptyContent>
        {dialogs}
      </Empty>
    )
  }

  return (
    <section className="flex flex-col gap-3">
      {scope && machine && local.data && <Delivery machine={machine} view={local.data} />}
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search variables" />
        <Button
          variant="ghost"
          size="lg"
          className="text-muted-foreground hover:text-foreground"
          onClick={() => (vault.unlocked ? lockVault() : setDialog("unlock"))}
        >
          {vault.unlocked ? (
            <LockSimpleOpenIcon data-icon="inline-start" />
          ) : (
            <LockSimpleIcon data-icon="inline-start" />
          )}
          {vault.unlocked ? "Lock" : "Unlock to see values"}
        </Button>
        <div className="ml-auto flex gap-2">{actions}</div>
      </div>

      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-[34%]" />
          <col />
          {scope && <col className="hidden w-[20%] md:table-column" />}
          <col className="hidden w-[16%] sm:table-column" />
          <col className="w-12" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Name</th>
            <th>Value</th>
            {scope && <th className="hidden md:table-cell">From</th>}
            <th className="hidden sm:table-cell">Updated</th>
            <th className="w-12" aria-label="Actions" />
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {loading
            ? Array.from({ length: 4 }, (_, index) => (
                <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
                  <td colSpan={columns}>
                    <Skeleton className="h-5 w-1/3" />
                  </td>
                </tr>
              ))
            : shown.map((row) => (
                <VariableRow
                  key={`${row.variable.scope}:${row.variable.name}`}
                  row={row}
                  machine={scope ? machine : undefined}
                  onNeedUnlock={() => setDialog("unlock")}
                />
              ))}
        </tbody>
      </table>
      {failed && (
        <p className="py-6 text-center text-sm text-muted-foreground">Couldn’t load variables. {failed.message}</p>
      )}
      {!loading && !failed && shown.length === 0 && (
        <p className="py-12 text-center text-sm text-muted-foreground">No variables match “{search.trim()}”</p>
      )}
      {dialogs}
    </section>
  )
}

/**
 * Whether a machine has the latest values: they're delivered encrypted, and the machine reports
 * which version it applied. Offline machines catch up when they're back.
 */
function Delivery({ machine, view }: { machine: Machine; view: ScopeView }) {
  const status = view.status ?? "pending"
  const [ok, label] =
    status === "applied"
      ? [true, `${machine.alias} has the latest values`]
      : status === "offline"
        ? [false, `${machine.alias} is offline. It gets the latest values when it’s back.`]
        : status === "pending"
          ? [false, `Waiting for ${machine.alias} to pick up the latest values…`]
          : [
              false,
              `${machine.alias} couldn’t apply the latest values${view.error_code ? ` (${view.error_code.replace(/_/g, " ")})` : ""}.`,
            ]
  return (
    <div className="flex items-center gap-2 text-sm text-muted-foreground">
      {status === "pending" ? <ArrowsClockwiseIcon className="size-3.5 animate-spin" /> : <StatusDot online={ok} />}
      <span>
        {label}
        {view.observed_at && status === "applied" && (
          <>
            {" · checked "}
            <Time iso={view.observed_at} relative />
          </>
        )}
      </span>
    </div>
  )
}

function VariableRow({
  row,
  machine,
  onNeedUnlock,
}: {
  row: Row
  /** Set in a machine's view; rows then say whether they're shared or the machine's own. */
  machine?: Machine
  onNeedUnlock: () => void
}) {
  const queryClient = useQueryClient()
  const vault = useVault()
  const { variable, overrides } = row
  const [dialog, setDialog] = useState<"edit" | "override" | "delete" | null>(null)
  const own = variable.scope === "machine"
  const target: Target =
    own && variable.machine_id ? { scope: "machine", machineId: variable.machine_id } : { scope: "global" }
  const value = vault.unlocked
    ? vault.values.get(valueKey(variable.scope, variable.machine_id, variable.name))
    : undefined
  const edit = (next: "edit" | "override") => (vault.unlocked ? setDialog(next) : onNeedUnlock())

  const entries: Entry[] = [
    { kind: "item", label: "Copy name", icon: CopyIcon, onSelect: () => void copyWithToast(variable.name, "Name") },
    ...(machine && !own
      ? ([
          { kind: "item", label: `Override on ${machine.alias}…`, icon: PlusIcon, onSelect: () => edit("override") },
        ] satisfies Entry[])
      : []),
    { kind: "separator" },
    {
      kind: "item",
      label: own && overrides ? "Remove override" : "Delete",
      icon: TrashIcon,
      destructive: true,
      onSelect: () => setDialog("delete"),
    },
  ]

  return (
    <RowMenus
      label={`Actions for ${variable.name}`}
      entries={entries}
      row={(actions) => (
        <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            {/* Clicking a variable edits its value. */}
            <button
              type="button"
              onClick={() => edit("edit")}
              className="block max-w-full truncate-x text-left font-mono text-code font-medium outline-none trim-cap after:absolute after:inset-0"
            >
              {variable.name}
            </button>
          </td>
          <td className="truncate font-mono text-code text-muted-foreground">
            {value !== undefined ? value || "(empty)" : "••••••••••••"}
          </td>
          {machine && (
            <td className="hidden md:table-cell">
              <span className="flex h-[1lh] min-w-0 items-center gap-2 text-muted-foreground">
                <span className="truncate-x trim-cap">{own ? `Only ${machine.alias}` : "All machines"}</span>
                {overrides && (
                  <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
                    Overrides
                  </Badge>
                )}
              </span>
            </td>
          )}
          <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
            <span className="relative z-10">
              <Time iso={variable.updated_at} relative />
            </span>
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
      dialogs={
        <>
          {dialog === "edit" && (
            <VariableDialog target={target} name={variable.name} value={value} onClose={() => setDialog(null)} />
          )}
          {dialog === "override" && machine && (
            <VariableDialog
              target={{ scope: "machine", machineId: machine.id }}
              name={variable.name}
              onClose={() => setDialog(null)}
            />
          )}
          <ConfirmAction
            open={dialog === "delete"}
            onOpenChange={(open) => setDialog(open ? "delete" : null)}
            title={own && overrides ? `Remove the override of ${variable.name}?` : `Delete ${variable.name}?`}
            description={
              own && overrides
                ? `${machine?.alias ?? "The machine"} goes back to the value every machine gets.`
                : own
                  ? `It’s removed from ${machine?.alias ?? "the machine"}. Programs started afterwards won’t see it.`
                  : "It’s removed from every machine. Programs started afterwards won’t see it."
            }
            action={own && overrides ? "Remove override" : "Delete"}
            pending="Removing…"
            mutation={{
              mutationFn: () => saveVariables(target, [{ name: variable.name, value: null }]),
              onSuccess: () => queryClient.invalidateQueries({ queryKey: ["environment"] }),
            }}
          />
        </>
      }
    />
  )
}
