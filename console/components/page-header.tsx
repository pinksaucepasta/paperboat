import Link from "next/link"
import { Children, Fragment } from "react"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { SidebarTrigger } from "@/components/ui/sidebar"
import { WorkspaceSwitcher } from "@/components/workspace-switcher"
import { CommandPalette } from "@/components/command-palette"

/** The bar at the top of every page: where you are, and which machine you are looking at. */
export function PageHeader({ children, actions }: { children: React.ReactNode; actions?: React.ReactNode }) {
  const crumbs = [<WorkspaceSwitcher key="workspace" />, ...Children.toArray(children)]
  return (
    <header className="sticky top-0 z-30 flex h-(--header-height) shrink-0 items-center gap-2 border-b bg-background/80 px-4 backdrop-blur md:px-6">
      {/* A full-size touch target on phones; the sidebar is a sheet there. */}
      <SidebarTrigger size="icon" className="-ml-2 size-10 md:hidden [&_svg]:size-5" />
      <Breadcrumb className="min-w-0 flex-1">
        <BreadcrumbList className="flex-nowrap text-sm">
          {crumbs.map((crumb, index) => (
            <Fragment key={index}>
              {index > 0 && <BreadcrumbSeparator />}
              <BreadcrumbItem className="min-w-0">{crumb}</BreadcrumbItem>
            </Fragment>
          ))}
        </BreadcrumbList>
      </Breadcrumb>
      {actions}
      <CommandPalette />
    </header>
  )
}

export function Crumb({ children }: { children: React.ReactNode }) {
  return <BreadcrumbPage className="truncate font-medium">{children}</BreadcrumbPage>
}

export function CrumbLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <BreadcrumbLink className="truncate" render={<Link href={href} />}>
      {children}
    </BreadcrumbLink>
  )
}
