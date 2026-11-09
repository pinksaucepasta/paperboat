import { Section } from "@/components/section"
import { Empty, EmptyHeader, EmptyMedia } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

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

/** Two-line list rows (a name over a detail line) while they load. */
export function ListSkeleton({ rows }: { rows: number }) {
  return (
    <div className="flex flex-col border-t border-divider">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="flex min-h-12 flex-col justify-center gap-2 border-b border-divider py-3">
          <Skeleton className="h-4 w-40" />
          <Skeleton className="h-3.5 w-80 max-w-full" />
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
