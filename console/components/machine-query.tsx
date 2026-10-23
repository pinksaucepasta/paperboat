"use client"

import { useSearchParams } from "next/navigation"
import { createContext, Suspense, useContext, useEffect, useState } from "react"

/**
 * The machines page's search text (which also carries `key:value` filters, see
 * `lib/machine-filter.ts`), shared so the stat cards above the table can set filters on it.
 */
const MachineQuery = createContext<[string, (query: string) => void] | null>(null)
const UrlPage = createContext(1)

type Url = { q: string; page: number }

/**
 * Starts from `?q=` (a tag link such as /?q=tag:prod), so pages can link to a filtered list; typing
 * takes over until a link brings another `?q=`. The URL is followed through navigations, because
 * Next keeps this page's state when it is left and opened again.
 */
export function MachineQueryProvider({ children }: { children: React.ReactNode }) {
  const [url, setUrl] = useState<Url>({ q: "", page: 1 })
  const [typed, setTyped] = useState<{ text: string; under: string } | null>(null)
  const query = typed && typed.under === url.q ? typed.text : url.q
  const state: [string, (query: string) => void] = [query, (text) => setTyped({ text, under: url.q })]
  return (
    <MachineQuery value={state}>
      <UrlPage value={url.page}>
        {/* Its own boundary: the server renders the page without the URL's search. */}
        <Suspense fallback={null}>
          <UrlParams onChange={setUrl} />
        </Suspense>
        {children}
      </UrlPage>
    </MachineQuery>
  )
}

function UrlParams({ onChange }: { onChange: (url: Url) => void }) {
  const params = useSearchParams()
  const q = params.get("q") ?? ""
  const page = Number(params.get("page")) || 1
  useEffect(() => onChange({ q, page }), [q, page, onChange])
  return null
}

export function useMachineQuery(): [string, (query: string) => void] {
  const state = useContext(MachineQuery)
  if (!state) throw new Error("useMachineQuery needs MachineQueryProvider")
  return state
}

/** The table's page from `?page=` (the table writes it with history.replaceState, which Next follows). */
export const useUrlPage = () => useContext(UrlPage)
