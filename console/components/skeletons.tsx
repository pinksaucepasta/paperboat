import { Section } from "@/components/section"
import { Empty, EmptyHeader, EmptyMedia } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

/* Loading states shaped like what they stand in for, so nothing moves when the data lands. */

/** A Section while its data loads: its title over `rows` label–value rows the size of `Row`. */
export function SectionSkeleton({ title, rows }: { title: string; rows: number }) {
  return (
    <Section title={title}>
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2">
          <Skeleton className="h-4 w-28" />
          <Skeleton className="h-4 w-36" />
        </div>
      ))}
    </Section>
  )
}

/** Two-line list rows (a name over a detail line, `min-h-14` like the lists) while they load. */
export function ListSkeleton({ rows, rowClassName = "min-h-14" }: { rows: number; rowClassName?: string }) {
  return (
    <div className="flex flex-col border-t border-divider">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className={cn("flex flex-col justify-center border-b border-divider py-2", rowClassName)}>
          <Skeleton className="my-0.5 h-4 w-40" />
          <Skeleton className="my-0.5 h-4 w-80 max-w-full" />
        </div>
      ))}
    </div>
  )
}

/** A centered `Empty` (icon, title, line) while it is not yet known which one shows. */
export function EmptySkeleton() {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Skeleton className="size-full" />
        </EmptyMedia>
        <Skeleton className="h-4 w-40" />
        <Skeleton className="h-4 w-72 max-w-full" />
      </EmptyHeader>
    </Empty>
  )
}

/**
 * Table rows while they load: one cell per column carrying its header cell's classes (pass `false`
 * for a column not shown), so columns hidden at this width stay hidden and the widths hold. The
 * placeholder sits in column `at`.
 */
export function TableRowsSkeleton({ rows, cells, at = 0 }: { rows: number; cells: (string | false)[]; at?: number }) {
  const shown = cells.filter((cell) => cell !== false)
  return Array.from({ length: rows }, (_, row) => (
    <tr key={row} className="[&>td]:h-14 [&>td]:px-3">
      {shown.map((className, cell) => (
        <td key={cell} className={className}>
          {cell === at && <Skeleton className="h-5 w-32 max-w-full" />}
        </td>
      ))}
    </tr>
  ))
}
