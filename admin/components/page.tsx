import { SidebarTrigger } from "@/components/ui/sidebar"

/** The bar at the top of every page, and its content column. */
export function Page({ title, description, actions, children }: { title: string; description?: string; actions?: React.ReactNode; children: React.ReactNode }) {
  return (
    <>
      <header className="sticky top-0 z-30 flex h-(--header-height) shrink-0 items-center gap-2 border-b bg-background/80 px-4 backdrop-blur md:px-6">
        <SidebarTrigger size="icon" className="-ml-2 size-10 md:hidden [&_svg]:size-5" />
        <h1 className="min-w-0 flex-1 truncate text-sm font-medium">{title}</h1>
        {actions}
      </header>
      <main className="mx-auto flex w-full max-w-6xl flex-col gap-4 px-4 py-6 md:px-6">
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
        {children}
      </main>
    </>
  )
}

export function Panel({ title, children, action }: { title: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-2">
      <div className="flex min-h-8 items-center gap-2">
        <h2 className="text-sm font-medium">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  )
}

/** Label–value rows for one record. */
export function Facts({ rows }: { rows: [string, React.ReactNode][] }) {
  return (
    <dl className="grid grid-cols-[minmax(8rem,12rem)_1fr] border-t border-divider text-sm">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="border-b border-divider py-2 pr-3 text-muted-foreground">{label}</dt>
          <dd className="min-w-0 break-words border-b border-divider py-2">{value ?? "—"}</dd>
        </div>
      ))}
    </dl>
  )
}

export function Stat({ label, value, note }: { label: string; value: React.ReactNode; note?: string }) {
  return (
    <div className="surface flex flex-col gap-1 rounded-lg border p-4" data-stat={label}>
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="text-2xl font-medium tabular-nums">{value}</span>
      {note && <span className="text-xs text-muted-foreground">{note}</span>}
    </div>
  )
}
