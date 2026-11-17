"use client"

import Link from "next/link"
import { usePathname, useRouter } from "next/navigation"
import { useEffect } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { BrandBar } from "@/components/brand-bar"
import { ErrorBlock } from "@/components/error-block"
import { Button } from "@/components/ui/button"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  useSidebar,
} from "@/components/ui/sidebar"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError, operatorQuery, post, useApiQuery, type Operator } from "@/lib/api"
import { sectionsFor } from "@/lib/sections"

/**
 * The operator panel: signed-in operators only. A missing or ended session goes to sign-in; any other failure of
 * `/v1/operator/me` is shown with its reference instead of a redirect (an outage is not a sign-out).
 */
export function Shell({ children }: { children: React.ReactNode }) {
  const router = useRouter()
  const pathname = usePathname()
  const me = useApiQuery<Operator>(operatorQuery.key, operatorQuery.path)
  const ended = (me.error instanceof ApiError && me.error.status === 401) || (me.data !== undefined && me.data.mfa !== "verified")
  useEffect(() => {
    if (ended) router.replace(`/login?return_to=${encodeURIComponent(pathname)}`)
  }, [ended, router, pathname])
  if (me.isError && !ended) return <ErrorBlock what="your session" error={me.error} retry={() => me.refetch()} />
  if (!me.data || ended) {
    return (
      <div className="flex h-svh items-center justify-center" data-state="loading">
        <Skeleton className="h-4 w-40" />
      </div>
    )
  }
  return (
    <SidebarProvider>
      <PanelSidebar operator={me.data} />
      <SidebarInset className="min-w-0">{children}</SidebarInset>
    </SidebarProvider>
  )
}

function PanelSidebar({ operator }: { operator: Operator }) {
  const { setOpenMobile } = useSidebar()
  const pathname = usePathname()
  const router = useRouter()
  const client = useQueryClient()
  return (
    <Sidebar>
      <SidebarHeader className="brand-bar h-(--header-height) justify-center overflow-hidden border-b px-4 py-0 text-white">
        <BrandBar onNavigate={() => setOpenMobile(false)} />
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup className="px-2 py-2">
          <SidebarMenu>
            {sectionsFor(operator.role).map(({ href, title, icon: Icon }) => {
              const active = pathname.startsWith(href)
              return (
                <SidebarMenuItem key={href}>
                  <SidebarMenuButton isActive={active} render={<Link href={href} onClick={() => setOpenMobile(false)} />}>
                    <Icon weight={active ? "fill" : "regular"} />
                    <span>{title}</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              )
            })}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter className="gap-1 border-t p-3 text-xs">
        <span className="truncate font-medium" data-operator-email>{operator.email}</span>
        <span className="text-muted-foreground">Role: {operator.role}</span>
        <Link className="text-muted-foreground underline-offset-2 hover:underline" href="/security" onClick={() => setOpenMobile(false)}>
          Security
        </Link>
        <Button
          variant="outline"
          size="sm"
          onClick={async () => {
            await post("/v1/operator/logout").catch(() => {})
            client.clear()
            router.replace("/login")
          }}
        >
          Sign out
        </Button>
      </SidebarFooter>
    </Sidebar>
  )
}
