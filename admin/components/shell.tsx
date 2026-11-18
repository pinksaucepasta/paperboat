"use client"

import Link from "next/link"
import { usePathname, useRouter } from "next/navigation"
import { useEffect } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { BrandBar } from "@/components/brand-bar"
import { ErrorBlock } from "@/components/error-block"
import { DotsThreeVerticalIcon, MonitorIcon, MoonIcon, ShieldCheckIcon, SignOutIcon, SunIcon } from "@phosphor-icons/react"
import { useTheme } from "next-themes"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
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
import { navigationFor } from "@/lib/sections"

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
  return (
    <Sidebar>
      <SidebarHeader className="brand-bar h-(--header-height) justify-center overflow-hidden border-b px-4 py-0 text-white">
        <BrandBar onNavigate={() => setOpenMobile(false)} />
      </SidebarHeader>
      <SidebarContent>
        <div className="flex flex-col py-2">
          {navigationFor(operator.role).map((group, index) => (
            <SidebarGroup key={group.label ?? index} className="px-2 py-1">
              {group.label && <SidebarGroupLabel>{group.label}</SidebarGroupLabel>}
              <SidebarMenu>
                {group.items.map(({ href, title, icon: Icon }) => {
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
          ))}
        </div>
      </SidebarContent>
      <SidebarFooter className="gap-0 p-0">
        <div className="border-t p-2">
          <OperatorMenu operator={operator} />
        </div>
      </SidebarFooter>
    </Sidebar>
  )
}

const themes = [
  { value: "system", label: "System", icon: MonitorIcon },
  { value: "light", label: "Light", icon: SunIcon },
  { value: "dark", label: "Dark", icon: MoonIcon },
] as const

/** The console's user menu: who is signed in, Security, Theme and Sign out. */
function OperatorMenu({ operator }: { operator: Operator }) {
  const { setOpenMobile } = useSidebar()
  const router = useRouter()
  const client = useQueryClient()
  const { theme, setTheme } = useTheme()
  const ThemeIcon = themes.find((item) => item.value === theme)?.icon ?? MonitorIcon
  const initials = operator.email.slice(0, 1).toUpperCase()
  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger render={<SidebarMenuButton size="lg" />}>
            <Avatar className="size-8">
              <AvatarFallback className="bg-primary text-xs font-medium text-primary-foreground">{initials}</AvatarFallback>
            </Avatar>
            <span className="flex min-w-0 flex-1 flex-col text-left leading-tight">
              <span className="truncate font-medium" data-operator-email>
                {operator.email}
              </span>
              <span className="truncate text-xs text-muted-foreground capitalize">{operator.role}</span>
            </span>
            <DotsThreeVerticalIcon weight="bold" className="text-muted-foreground" />
          </DropdownMenuTrigger>
          <DropdownMenuContent side="top" align="start" className="w-(--anchor-width) min-w-56">
            <DropdownMenuItem render={<Link href="/security" onClick={() => setOpenMobile(false)} />}>
              <ShieldCheckIcon /> Security
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuSub>
              <DropdownMenuSubTrigger>
                <ThemeIcon /> Theme
              </DropdownMenuSubTrigger>
              <DropdownMenuSubContent className="min-w-36">
                <DropdownMenuRadioGroup value={theme ?? "system"} onValueChange={(next) => setTheme(String(next))}>
                  {themes.map(({ value, label, icon: ItemIcon }) => (
                    <DropdownMenuRadioItem key={value} value={value} closeOnClick={false}>
                      <ItemIcon /> {label}
                    </DropdownMenuRadioItem>
                  ))}
                </DropdownMenuRadioGroup>
              </DropdownMenuSubContent>
            </DropdownMenuSub>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              onClick={async () => {
                await post("/v1/operator/logout").catch(() => {})
                client.clear()
                router.replace("/login")
              }}
            >
              <SignOutIcon /> Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
