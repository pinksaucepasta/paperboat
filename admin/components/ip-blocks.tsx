"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { TextField } from "@/components/fields"
import { Mono, StatusBadge } from "@/components/filters"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { operatorAction, type Data } from "@/lib/api"
import { useNow } from "@/lib/use-now"

export type IPBlock = Data<"/v1/admin/ip-blocks">["blocks"][number]

/** An <input type=datetime-local> value as RFC 3339 UTC: "" when empty, undefined when it is not a future time. */
const endsAt = (local: string, now: number): string | undefined => {
  if (!local) return ""
  const parsed = Date.parse(local)
  return Number.isNaN(parsed) || parsed <= now ? undefined : new Date(parsed).toISOString()
}

/** Block an address or range from sign-up, sign-in and CLI login, with an optional end. */
export function BlockNetwork() {
  const [cidr, setCidr] = useState("")
  const [ends, setEnds] = useState("")
  const now = useNow()
  const ending = endsAt(ends, now)
  const cidrOk = /^[0-9a-fA-F:.]+(\/\d{1,3})?$/.test(cidr.trim()) && cidr.trim().length <= 64
  return (
    <ActionDialog
      title="Block a network"
      description="Sign-up, sign-in and CLI login from this address or range are refused with a clear error. Sessions that already exist stay. The address is the one the server sees behind its proxy."
      target="ip-blocks"
      submitLabel="Block network"
      valid={cidrOk && ending !== undefined}
      trigger={(open) => (
        <Button size="lg" onClick={open} data-action="block-network">
          Block a network
        </Button>
      )}
      run={(h) => operatorAction("POST", "/v1/admin/ip-blocks", h, { cidr: cidr.trim(), ...(ending ? { ends_at: ending } : {}) })}
      onDone={() => {
        setCidr("")
        setEnds("")
      }}
    >
      <TextField label="Address or range" value={cidr} onChange={setCidr} placeholder="203.0.113.0/24 or 2001:db8::/48" hint="IPv4 up to /8, IPv6 up to /16." />
      <TextField label="Ends" optional type="datetime-local" value={ends} onChange={setEnds} hint={ending === undefined ? "Pick a time in the future." : "Empty keeps it until you lift it."} />
    </ActionDialog>
  )
}

/** The state of one block and, while it runs, the way to lift it. */
export function IPBlockState({ block }: { block: IPBlock }) {
  return (
    <span className="inline-flex items-center gap-3">
      <StatusBadge state={block.running ? "blocked" : block.revoked_at ? "lifted" : "expired"} />
      {block.running && (
        <ActionDialog
          title={`Lift the block on ${block.cidr}`}
          description="Sign-up, sign-in and CLI login from this range work again at once."
          target={block.id}
          submitLabel="Lift block"
          trigger={(open) => (
            <Button variant="outline" size="xs" onClick={open} data-action="lift-block">
              Lift
            </Button>
          )}
          run={(h) => operatorAction("POST", `/v1/admin/ip-blocks/${encodeURIComponent(block.id)}/remove`, h, {})}
        />
      )}
    </span>
  )
}

export const blockEnd = (b: IPBlock) => (b.expires_at ? <Time iso={b.expires_at} /> : "Until lifted")
export const blockRange = (b: IPBlock) => <Mono>{b.cidr}</Mono>
