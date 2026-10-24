import { Button } from "@/components/ui/button"

/** Previous/Next under a server-paged list, with "51–100 of N"; nothing when one page holds it all. */
export function ListPager({
  page,
  size,
  total,
  go,
}: {
  page: number
  size: number
  total: number
  go: (page: number) => void
}) {
  if (total <= size) return null
  const from = (page - 1) * size
  return (
    <nav className="flex items-center justify-end gap-3 py-3 text-sm text-muted-foreground" aria-label="Pages">
      <span className="tabular-nums" data-list-range>
        {from + 1}–{Math.min(from + size, total)} of {total}
      </span>
      <Button variant="outline" size="sm" disabled={page === 1} onClick={() => go(page - 1)}>
        Previous
      </Button>
      <Button variant="outline" size="sm" disabled={from + size >= total} onClick={() => go(page + 1)}>
        Next
      </Button>
    </nav>
  )
}
