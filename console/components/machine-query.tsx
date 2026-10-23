"use client"

import { createContext, useContext, useState, useSyncExternalStore } from "react"

/**
 * The machines page's search text (which also carries `key:value` filters, see
 * `lib/machine-filter.ts`), shared so the stat cards above the table can set filters on it.
 */
const MachineQuery = createContext<[string, (query: string) => void] | null>(null)

const noSubscription = () => () => {}

/**
 * Starts from `?q=` (a tag link such as /?q=tag:prod), so pages can link to a filtered list. The
 * URL is read after hydration (the server renders an empty search), then typing takes over.
 */
export function MachineQueryProvider({ children }: { children: React.ReactNode }) {
  const fromUrl = useSyncExternalStore(
    noSubscription,
    () => new URLSearchParams(window.location.search).get("q") ?? "",
    () => "",
  )
  const [typed, setTyped] = useState<string | null>(null)
  const state: [string, (query: string) => void] = [typed ?? fromUrl, setTyped]
  return <MachineQuery value={state}>{children}</MachineQuery>
}

export function useMachineQuery(): [string, (query: string) => void] {
  const state = useContext(MachineQuery)
  if (!state) throw new Error("useMachineQuery needs MachineQueryProvider")
  return state
}
