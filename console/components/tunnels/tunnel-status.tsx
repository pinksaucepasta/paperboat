"use client"

import type { Tunnel } from "@/lib/tunnels"
import { StatusDot } from "@/components/machine-status"

/**
 * A tunnel's state in a few words, from what the list already carries: whether it's paused,
 * and the server's summary of its health (the same codes `/status` expands on).
 */
export function tunnelState(tunnel: Tunnel): { healthy: boolean; label: string } {
  if (tunnel.desired_state === "paused") return { healthy: false, label: "Paused" }
  switch (tunnel.summary_code) {
    case "ready":
      return { healthy: true, label: "Healthy" }
    case "pending":
      return { healthy: false, label: "Connecting" }
    case "route_unavailable":
      return { healthy: false, label: "Can’t reach the service" }
    case "custom_domain_required":
      return { healthy: false, label: "Needs a domain" }
    case "expired":
    case "tunnel_expired":
      return { healthy: false, label: "Expired" }
    default:
      return { healthy: false, label: "Unknown" }
  }
}

export function TunnelStatus({ tunnel }: { tunnel: Tunnel }) {
  const { healthy, label } = tunnelState(tunnel)
  return (
    <span className="flex items-center gap-2">
      <StatusDot online={healthy} />
      {label}
    </span>
  )
}
