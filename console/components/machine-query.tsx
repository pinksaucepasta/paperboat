"use client"

import { createContext, useContext, useState } from "react"

/**
 * The machines page's search text (which also carries `key:value` filters, see
 * `lib/machine-filter.ts`), shared so the stat cards above the table can set filters on it.
 */
const MachineQuery = createContext<[string, (query: string) => void] | null>(null)

export function MachineQueryProvider({ children }: { children: React.ReactNode }) {
  const state = useState("")
  return <MachineQuery value={state}>{children}</MachineQuery>
}

export function useMachineQuery(): [string, (query: string) => void] {
  const state = useContext(MachineQuery)
  if (!state) throw new Error("useMachineQuery needs MachineQueryProvider")
  return state
}
