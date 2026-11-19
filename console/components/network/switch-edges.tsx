"use client"

import { useQueries } from "@tanstack/react-query"
import { useApiQuery } from "@/lib/api"
import { type LiveItem } from "@/lib/network"
import { domainsQuery, hostOf, tunnelsPageQuery } from "@/lib/tunnels"
import { State } from "@/components/domains/domain-list"
import { domainStates } from "@/components/tunnels/tunnel-hostnames"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useShellState } from "@/lib/use-shell-state"

/*
 * Changing between Paperboat's edges and your own servers strands what runs on the old ones, so the
 * server refuses while previews or tunnels run (409 previews_running) and names them. These are the dialog's two follow-ups: list what is running and stop it after a typed
 * confirmation, then, once the edges changed, say what to start again and how the domains are doing.
 */

export const CONFIRM_SWITCH = "switch edges"

const verb = { preview: "ends", tunnel: "is paused" } as const
const noun = { preview: "Preview", tunnel: "Tunnel" } as const

/** What the server named as running: each with what stopping does to it. */
function LiveList({ items, count }: { items: LiveItem[]; count: number }) {
  return (
    <ul className="flex flex-col gap-1.5 text-sm" aria-label="Running on the current edges">
      {items.map((item) => (
        <li key={`${item.kind}:${item.id}`} className="flex items-baseline gap-2">
          <span className="w-16 shrink-0 text-muted-foreground">{noun[item.kind]}</span>
          <span className="min-w-0 truncate font-medium">
            {item.kind === "preview" ? hostOf(item.name) : item.name}
          </span>
          <span className="ml-auto shrink-0 text-muted-foreground">{verb[item.kind]}</span>
        </li>
      ))}
      {count > items.length && (
        <li className="text-muted-foreground">and {count - items.length} more, stopped the same way</li>
      )}
    </ul>
  )
}

/** The refusal's follow-up: what is running, and "Stop these and switch" behind a typed confirmation. */
export function StopAndSwitch({
  items,
  count,
  pending,
  onConfirm,
}: {
  items: LiveItem[]
  count: number
  pending: boolean
  onConfirm: () => void
}) {
  const [typed, setTyped] = useShellState("network/switch-edges.tsx:typed", "")
  const confirmed = typed.trim() === CONFIRM_SWITCH
  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={(event) => {
        event.preventDefault()
        if (confirmed && !pending) onConfirm()
      }}
    >
      <p className="text-sm">These are running on the current edges and have to stop before you switch:</p>
      <LiveList items={items} count={count} />
      <p className="text-sm text-muted-foreground">
        Tunnels are paused, not deleted. After the switch, create the previews again and resume the tunnels.
      </p>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="confirm-switch" className="font-normal text-muted-foreground">
          Type <span className="font-mono font-medium text-foreground">{CONFIRM_SWITCH}</span> to confirm
        </FieldLabel>
        <Input
          id="confirm-switch"
          value={typed}
          onChange={(event) => setTyped(event.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
      <Button type="submit" variant="destructive" size="lg" disabled={!confirmed || pending}>
        {pending ? "Stopping and switching…" : "Stop these and switch"}
      </Button>
    </form>
  )
}

/** Tunnels past the first ten are on their own pages; the cutover list stays short. */
const listed = 10

/**
 * After the edges changed: what to start again, and per hostname of the workspace's tunnels whether its DNS
 * and certificate have caught up with the new edges (the server re-points them and checks again by itself).
 */
export function SwitchedEdges({
  workspace,
  stopped,
  toOwnServers,
  onClose,
}: {
  workspace: string
  stopped: LiveItem[]
  toOwnServers: boolean
  onClose: () => void
}) {
  const tunnels = useApiQuery(tunnelsPageQuery({ workspace, q: "", page: 1 }))
  const shown = (tunnels.data?.items ?? []).slice(0, listed)
  const domains = useQueries({ queries: shown.map((tunnel) => domainsQuery(tunnel.id)) })
  const rows = shown.flatMap((tunnel, index) =>
    (domains[index]?.data ?? []).map((domain) => ({ tunnel: tunnel.name, ...domain })),
  )
  const settled = rows.length > 0 && rows.every((row) => domainStates[row.state].tone === "ok")
  return (
    <div className="flex flex-col gap-4">
      {(stopped.length > 0 || toOwnServers) && (
        <p className="text-sm">
          {stopped.length > 0 && "Create the stopped previews again and resume the paused tunnels."}
          {stopped.length > 0 && toOwnServers && " "}
          {toOwnServers && "On your own servers, previews and tunnel hostnames need a custom domain."}
        </p>
      )}
      {rows.length > 0 && (
        <div className="flex flex-col gap-2">
          <p className="text-sm font-medium">Your domains on the new edges</p>
          <ul className="flex flex-col gap-1.5" aria-label="Domains on the new edges">
            {rows.map((row) => (
              <li key={row.id} className="flex items-center gap-2 text-sm">
                <span className="min-w-0 truncate font-medium">{row.hostname}</span>
                <span className="shrink-0 text-muted-foreground">{row.tunnel}</span>
                <span className="ml-auto">
                  <State {...domainStates[row.state]} />
                </span>
              </li>
            ))}
          </ul>
          {!settled && (
            <p className="text-sm text-muted-foreground">
              Paperboat points them at the new edges and checks again on its own. Until DNS catches up they won’t load:
              a change reaches visitors once the old record’s cache expires, usually within minutes, longer where a
              resolver or your DNS provider caches it.
            </p>
          )}
        </div>
      )}
      <Button size="lg" onClick={onClose}>
        Done
      </Button>
    </div>
  )
}
