import { describe, expect, test } from "bun:test"
import { addressChangeReason, tunnelQuery } from "@/lib/tunnels"

describe("address change", () => {
  test("every failure code the server sets has its own words", () => {
    const words = ["tunnel_address_not_ready", "subdomain_taken", "address_blocked", "tunnel_paused", "tunnel_deleted"].map(addressChangeReason)
    expect(new Set(words).size).toBe(5)
    expect(addressChangeReason(undefined)).toBe(addressChangeReason("tunnel_address_not_ready"))
  })

  test("the tunnel is polled faster only while it moves", () => {
    const interval = tunnelQuery("tun_1").refetchInterval as (query: { state: { data: unknown } }) => number
    const at = (state?: string) => interval({ state: { data: state ? { address_change: { state } } : {} } })
    expect(at("moving")).toBeLessThan(at())
    expect(at("failed")).toBe(at())
  })
})
