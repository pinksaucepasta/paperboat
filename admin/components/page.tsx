import Link from "next/link"
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from "@/components/ui/breadcrumb"
import { SidebarTrigger } from "@/components/ui/sidebar"

/**
 * The bar at the top of every page (a breadcrumb, as in the console) and the page's one content column. Section pages
 * have no h1 (the crumb names them); an object's page passes `parent` and shows its name as `ObjectTitle`.
 */
export function Page({
  title,
  parent,
  actions,
  children,
}: {
  title: React.ReactNode
  parent?: { href: string; title: string }
  actions?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <>
      <header className="sticky top-0 z-30 flex h-(--header-height) shrink-0 items-center gap-2 border-b bg-background/80 px-4 backdrop-blur md:px-6">
        <SidebarTrigger size="icon" className="-ml-2 size-10 md:hidden [&_svg]:size-5" />
        <Breadcrumb className="min-w-0 flex-1">
          <BreadcrumbList className="flex-nowrap text-sm">
            {parent && (
              <>
                <BreadcrumbItem className="min-w-0">
                  <BreadcrumbLink
                    className="truncate rounded-sm outline-none focus-visible:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
                    render={<Link href={parent.href} />}
                  >
                    {parent.title}
                  </BreadcrumbLink>
                </BreadcrumbItem>
                <BreadcrumbSeparator />
              </>
            )}
            <BreadcrumbItem className="min-w-0">
              <BreadcrumbPage className="truncate font-medium">{title}</BreadcrumbPage>
            </BreadcrumbItem>
          </BreadcrumbList>
        </Breadcrumb>
        {actions}
      </header>
      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-8 px-4 py-6 md:px-8 md:py-8">{children}</main>
    </>
  )
}

/** An object's name at the top of its page, as on the console's machine page. */
export function ObjectTitle({ children, detail }: { children: React.ReactNode; detail?: React.ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <h1 className="truncate font-heading text-2xl font-semibold tracking-tight">{children}</h1>
      {detail && <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">{detail}</div>}
    </div>
  )
}

/** A titled part of a page; `action` sits at the right of the heading. */
export function Panel({ title, children, action }: { title: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex min-h-8 flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-medium">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  )
}

/** Label–value rows for one record, two columns on wide screens (the console's machine Details). */
export function Facts({ rows }: { rows: [string, React.ReactNode][] }) {
  return (
    <dl className="grid border-t border-divider sm:grid-cols-2 sm:gap-x-10">
      {rows.map(([label, value]) => (
        <div key={label} className="flex min-h-11 min-w-0 items-center gap-4 border-b border-divider py-1 text-sm">
          <dt className="w-28 shrink-0 text-muted-foreground">{label}</dt>
          <dd className="min-w-0 flex-1 break-words">{value ?? <span className="text-muted-foreground">—</span>}</dd>
        </div>
      ))}
    </dl>
  )
}

/** A figure panel, like the console's usage stats. */
export function Stat({ label, value, note }: { label: string; value: React.ReactNode; note?: React.ReactNode }) {
  return (
    <div className="surface-pattern flex flex-col gap-2 p-5" data-stat={label}>
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <span className="font-heading text-2xl font-semibold tracking-tight tabular-nums">{value}</span>
      {note && <span className="text-xs text-muted-foreground">{note}</span>}
    </div>
  )
}

/** The console's Danger zone: a faint red panel, one row per action, each with a light chip opening its confirmation. */
export function DangerZone({ children }: { children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3" data-danger-zone>
      <div className="flex min-h-8 items-center">
        <h2 className="text-sm font-medium text-destructive">Danger zone</h2>
      </div>
      <div className="flex flex-col divide-y divide-destructive/10 rounded-lg bg-destructive/5 dark:bg-destructive/10">{children}</div>
    </section>
  )
}

export function DangerRow({ title, description, children }: { title: string; description: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-4 p-4">
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <p className="text-sm font-medium">{title}</p>
        <p className="text-sm text-muted-foreground">{description}</p>
      </div>
      {children}
    </div>
  )
}

/** The chip that opens a Danger zone confirmation. */
export const dangerChip = "bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
