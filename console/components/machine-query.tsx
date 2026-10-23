"use client"

import { createContext, useContext, useEffect, useState } from "react"

/**
 * The machines page's search text (which also carries `key:value` filters, see
 * `lib/machine-filter.ts`), shared so the stat cards above the table can set filters on it.
 */
const MachineQuery = createContext<[string, (query: string) => void] | null>(null)

/** The page's URL as it was when the page was last shown: `?q=` and `?page=`. */
export type MachineUrl = { q: string; page: number }
const UrlContext = createContext<MachineUrl>({ q: "", page: 1 })

const readUrl = (): MachineUrl => {
  const params = new URLSearchParams(window.location.search)
  return { q: params.get("q") ?? "", page: Number(params.get("page")) || 1 }
}

/**
 * Starts from `?q=` (a tag link such as /?q=tag:prod), so pages can link to a filtered list; typing
 * takes over until the page is opened again from a link. The URL is read whenever the page is
 * shown: on mount, and when Next shows the page it kept (its effects run again then). The server
 * renders without the URL's search.
 */
export function MachineQueryProvider({ children }: { children: React.ReactNode }) {
  const [url, setUrl] = useState<MachineUrl>({ q: "", page: 1 })
  const [typed, setTyped] = useState<{ text: string; under: MachineUrl } | null>(null)
  // Reads the URL each time the page is shown: Next re-runs effects when it shows the kept page.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => setUrl(readUrl()), [])
  const query = typed && typed.under === url ? typed.text : url.q
  const state: [string, (query: string) => void] = [query, (text) => setTyped({ text, under: url })]
  return (
    <MachineQuery value={state}>
      <UrlContext value={url}>{children}</UrlContext>
    </MachineQuery>
  )
}

export function useMachineQuery(): [string, (query: string) => void] {
  const state = useContext(MachineQuery)
  if (!state) throw new Error("useMachineQuery needs MachineQueryProvider")
  return state
}

/** The URL the page was opened with (changes each time the page is shown). */
export const useMachineUrl = () => useContext(UrlContext)
