"use client"

import { useState } from "react"
import { BellIcon } from "@phosphor-icons/react"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem } from "@/components/ui/sidebar"
import { Time } from "@/components/time"
import { useApiQuery, type Data } from "@/lib/api"
import { openAlerts } from "@/lib/host-health"
import { useNow } from "@/lib/use-now"

type Notice = Data<"/v1/admin/node-notices">["notices"][number]
const seenKey = "pb-admin-notices-seen"

/**
 * The admin bell: system notices about our global nodes (outdated, below the update floor, an update that finished
 * or failed). Operators have no read receipts, so this browser remembers the newest notice it showed.
 */
export function NoticesBell() {
  const notices = useApiQuery<Data<"/v1/admin/node-notices">>(["admin-notices"], "/v1/admin/node-notices", 60_000).data?.notices ?? []
  const reports = useApiQuery<Data<"/v1/admin/host-health">>(["admin-host-health"], "/v1/admin/host-health", 30_000).data?.reports
  const alerts = openAlerts(reports, useNow())
  const [seen, setSeen] = useState(() => (typeof window === "undefined" ? "" : (localStorage.getItem(seenKey) ?? "")))
  const unread = notices.filter((notice) => notice.sent_at > seen).length + alerts.length
  const open = (next: boolean) => {
    if (next && notices[0]) {
      localStorage.setItem(seenKey, notices[0].sent_at)
      setSeen(notices[0].sent_at)
    }
  }
  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <Popover onOpenChange={open}>
          <PopoverTrigger render={<SidebarMenuButton data-admin-notices={unread} />}>
            <BellIcon />
            <span>Notices</span>
            {unread > 0 && <span className="ml-auto rounded-full bg-primary px-1.5 text-xs text-primary-foreground">{unread}</span>}
          </PopoverTrigger>
          <PopoverContent side="right" align="end" className="w-80 p-0">
            {alerts.length > 0 && (
              <ul className="divide-y border-b" data-open-alerts={alerts.length}>
                {alerts.map((alert) => (
                  <li key={alert.id} className="space-y-1 p-3 text-sm" data-alert={alert.id}>
                    <p className={alert.level === "critical" ? "font-medium text-destructive" : "font-medium"}>{alert.title}</p>
                    <p className="text-muted-foreground">{alert.body}</p>
                    {alert.since && <p className="text-xs text-muted-foreground"><Time iso={alert.since} relative /></p>}
                  </li>
                ))}
              </ul>
            )}
            {notices.length === 0 ? (
              alerts.length === 0 && <p className="p-4 text-sm text-muted-foreground">No notices.</p>
            ) : (
              <ul className="max-h-96 divide-y overflow-y-auto">
                {notices.map((notice: Notice) => (
                  <li key={notice.id} className="space-y-1 p-3 text-sm">
                    <p className="font-medium">{notice.title}</p>
                    <p className="text-muted-foreground">{notice.body}</p>
                    <p className="text-xs text-muted-foreground"><Time iso={notice.sent_at} relative /></p>
                  </li>
                ))}
              </ul>
            )}
          </PopoverContent>
        </Popover>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
