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
 * A column of a loading table: its header cell's classes, and the shape of its loaded cell — a bar
 * of width `bar` on the 20 px line (`dot` adds a status dot after it), an optional smaller second
 * line of width `second`, or nothing (`empty`, for the "…" column). `false`: not shown.
 */
export type SkeletonColumn =
  false | { className?: string; bar?: string; second?: string; dot?: boolean; empty?: boolean }

/**
 * Table rows while they load: one cell per column carrying its header cell's classes (so columns
 * hidden at this width stay hidden and the widths hold), each with a placeholder shaped like the
 * loaded cell. Pass the last known row count; the default fills a large screen.
 */
export function TableRowsSkeleton({ rows = 12, columns }: { rows?: number; columns: SkeletonColumn[] }) {
  const shown = columns.filter((column) => column !== false)
  return Array.from({ length: rows }, (_, row) => (
    <tr key={row} className="[&>td]:h-14 [&>td]:px-3">
      {shown.map((column, index) => {
        const end = column.className?.includes("text-right")
        return (
          <td key={index} className={column.className}>
            {!column.empty && (
              <>
                <div className={cn("flex h-5 items-center gap-2", end && "justify-end")}>
                  <Skeleton className={cn("h-3 max-w-full", column.bar ?? "w-24")} />
                  {column.dot && <Skeleton className="size-2 shrink-0 rounded-full" />}
                </div>
                {column.second && (
                  <Skeleton className={cn("my-[3px] h-2.5 max-w-full", column.second, end && "ml-auto")} />
                )}
              </>
            )}
          </td>
        )
      })}
    </tr>
  ))
}
