"use client"

import { ActionDialog } from "@/components/action-dialog"
import { Mono, StatusBadge } from "@/components/filters"
import { Panel } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

type RecentIPs = Data<"/v1/admin/accounts/{account_id}/ips">

/** The addresses an account recently signed in, signed up or logged in from (the audit log's), for operators only. */
export function RecentAddresses({ id }: { id: string }) {
  const admin = useOperator()?.role === "admin"
  const read = useApiQuery<RecentIPs>(["account-ips", id], admin ? `/v1/admin/accounts/${encodeURIComponent(id)}/ips` : null)
  if (!admin || !read.data) return null
  const ips = read.data.ips
  return (
    <Panel title="Recent addresses">
      {ips.length === 0 ? (
        <p className="py-3 text-sm text-muted-foreground">No sign-in addresses are on record for this account.</p>
      ) : (
        <ul className="text-sm" data-recent-addresses>
          {ips.map((v) => (
            <li key={v.ip} className="flex min-h-12 flex-wrap items-center gap-x-4 gap-y-1 border-b border-divider py-1" data-ip={v.ip}>
              <Mono>{v.ip}</Mono>
              <span className="min-w-0 flex-1 text-muted-foreground">
                {v.count} sign-in{v.count === 1 ? "" : "s"} · last <Time iso={v.last_seen} />
              </span>
              {v.blocked ? (
                <StatusBadge state="blocked" />
              ) : (
                <ActionDialog
                  title={`Block ${v.ip}`}
                  description="Sign-up, sign-in and CLI login from this address are refused. Sessions that exist stay. Lift it under IP blocks."
                  target="ip-blocks"
                  submitLabel="Block address"
                  trigger={(open) => (
                    <Button variant="outline" size="xs" onClick={open} data-action="block-ip">
                      Block
                    </Button>
                  )}
                  run={(h) => operatorAction("POST", "/v1/admin/ip-blocks", h, { cidr: v.ip })}
                />
              )}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}
