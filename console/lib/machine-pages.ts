/**
 * The workspace's machines, page by page (the server caps a page at 200), so lists and pickers
 * never silently miss machines past the first page. Bounded: at most `maxMachines` are loaded, and
 * `complete` says whether that was all of them. Shared by the browser query and the server prefetch.
 */
export const pageSize = 200
export const maxMachines = 2000

export type MachinePages<T> = { items: T[]; total: number; complete: boolean }

export async function fetchAllPages<T>(
  fetchPage: (params: Record<string, string>) => Promise<{ items: T[] | null; pagination: { total: number } } | null | undefined>,
  workspace: string,
): Promise<MachinePages<T> | null> {
  const items: T[] = []
  let total = 0
  for (let offset = 0; offset < maxMachines; offset += pageSize) {
    const page = await fetchPage({ limit: String(pageSize), offset: String(offset), workspace })
    if (!page) return offset === 0 ? null : { items, total, complete: false }
    // An empty list can arrive as null (a Go nil slice); it means no items, not a broken page.
    const got = page.items ?? []
    items.push(...got)
    total = page.pagination.total
    if (got.length < pageSize || items.length >= total) break
  }
  return { items, total, complete: items.length >= total }
}

/**
 * The machines list both the browser and the server prefetch load: those in use from the default
 * list (its total is what `complete` measures), then the revoked and disconnected ones, which the
 * server lists only with ?state=revoked (for the Revoked filter).
 */
export async function fetchMachineList<T>(
  fetchPage: (params: Record<string, string>) => Promise<{ items: T[] | null; pagination: { total: number } } | null | undefined>,
  workspace: string,
): Promise<MachinePages<T> | null> {
  const [live, revoked] = await Promise.all([
    fetchAllPages(fetchPage, workspace),
    fetchAllPages((params) => fetchPage({ ...params, state: "revoked" }), workspace),
  ])
  return live && { ...live, items: [...live.items, ...(revoked?.items ?? [])] }
}
