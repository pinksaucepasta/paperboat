"use client"

import { queryOptions } from "@tanstack/react-query"
import { useMemo, useSyncExternalStore } from "react"
import { ApiError, clientConfigQuery, send, useApiQuery } from "@/lib/api"

/**
 * Domains machines are reachable under (`<name>.<domain>`). Paperboat's own domain is always
 * there; people can add their own and pick which one is shown first. There is no server API yet,
 * so this is a per-browser stand-in (see NOTES.md, "Machine domains"); only this module changes
 * when the API lands.
 */
export type MachineDomains = { domains: { name: string; managed: boolean }[]; primary: string }

type Stored = { added: string[]; primary: string }

const key = "pb-machine-domains"
const initial: Stored = { added: [], primary: "" }
const listeners = new Set<() => void>()
let cache: { raw: string | null; value: Stored } | undefined

function readRaw(): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

/** Paperboat's own domain (from the server's client configuration) first, then the added ones. */
function build(stored: Stored, managed: string | undefined): MachineDomains {
  const domains = [
    ...(managed ? [{ name: managed, managed: true }] : []),
    ...stored.added.map((name) => ({ name, managed: false })),
  ]
  const primary = domains.some((domain) => domain.name === stored.primary) ? stored.primary : (managed ?? "")
  return { domains, primary }
}

function stored(raw: string | null): Stored {
  if (!raw) return initial
  try {
    const value = JSON.parse(raw)
    if (Array.isArray(value.added) && typeof value.primary === "string") return value
  } catch {}
  return initial
}

function snapshot(): Stored {
  const raw = readRaw()
  if (!cache || cache.raw !== raw) cache = { raw, value: stored(raw) }
  return cache.value
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => event.key === key && listener()
  window.addEventListener("storage", onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener("storage", onStorage)
  }
}

function write(next: Stored) {
  try {
    localStorage.setItem(key, JSON.stringify(next))
  } catch {
    // Storage unavailable (private mode); the change lasts until reload.
  }
  listeners.forEach((listener) => listener())
}

export function useMachineDomains(): MachineDomains {
  const added = useSyncExternalStore(subscribe, snapshot, () => initial)
  const managed = useApiQuery(clientConfigQuery).data?.local_access_base_domain
  return useMemo(() => build(added, managed), [added, managed])
}

/** Every address a machine name has, the primary domain first. */
export function useMachineHostnames(alias: string): string[] {
  const { domains, primary } = useMachineDomains()
  return [primary, ...domains.map((domain) => domain.name).filter((name) => name !== primary)].map(
    (domain) => `${alias}.${domain}`,
  )
}

const domainPattern = /^(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/

export function domainProblem(name: string, current: MachineDomains): string | undefined {
  if (!domainPattern.test(name)) return "Enter a domain like example.com"
  if (current.domains.some((domain) => domain.name === name)) return "Already added"
}

export function addDomain(name: string) {
  const current = stored(readRaw())
  write({ ...current, added: [...current.added, name] })
}

export function removeDomain(name: string) {
  const current = stored(readRaw())
  write({
    added: current.added.filter((domain) => domain !== name),
    primary: current.primary === name ? "" : current.primary,
  })
}

export function setPrimaryDomain(name: string) {
  write({ ...stored(readRaw()), primary: name })
}

/** Whether `*.domain` resolves to Paperboat's local gateway, checked by the console's server. */
export type DomainCheck = { zone?: string } & (
  | { status: "ready" }
  | { status: "wrong_address"; addresses: string[] }
  | { status: "not_found" }
  | { status: "unavailable" }
)

export const gatewayAddress = "127.100.0.1"

/**
 * The record's Name as a DNS provider wants it: relative to the zone (`*` at the zone apex,
 * `*.home` for `home.example.net` in `example.net`). Without a known zone, assume the last two
 * labels are the zone, which is right for most domains.
 */
export function recordName(domain: string, zone?: string): string {
  const apex = zone && (domain === zone || domain.endsWith(`.${zone}`)) ? zone : domain.split(".").slice(-2).join(".")
  const prefix = domain === apex ? "" : domain.slice(0, -(apex.length + 1))
  return prefix ? `*.${prefix}` : "*"
}

export const domainCheckQuery = (domain: string) =>
  queryOptions({
    queryKey: ["domain-check", domain],
    queryFn: async (): Promise<DomainCheck> => {
      const response = await send(`/api/domain-check?${new URLSearchParams({ domain })}`)
      const body = (await response.json().catch(() => null)) as {
        data?: DomainCheck
        error?: { code?: string; message?: string }
      } | null
      if (!response.ok || !body?.data) {
        throw new ApiError(
          response.status,
          body?.error?.code ?? "unavailable",
          body?.error?.message ?? "Couldn’t check this domain.",
        )
      }
      return body.data
    },
    staleTime: 5 * 60_000,
    retry: false,
  })
