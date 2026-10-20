"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { navigationGroups, type Section } from "@/lib/sections"
import { UserMenu } from "@/components/user-menu"
import { BrandBar } from "@/components/brand-bar"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar"

/** `navigation` is passed in so the server layout owns its Suspense boundary. */
export function AppSidebar({ navigation }: { navigation: React.ReactNode }) {
  const { setOpenMobile } = useSidebar()
  return (
    <Sidebar>
      {/* Same height as the page header; the logo's gradient, with the boat animation in BrandBar. */}
      <SidebarHeader className="brand-bar h-(--header-height) justify-center overflow-hidden border-b px-4 py-0 text-white">
        <BrandBar onNavigate={() => setOpenMobile(false)} />
      </SidebarHeader>
      <SidebarContent>
        <div className="flex flex-col py-2">{navigation}</div>
      </SidebarContent>
      <SidebarFooter className="gap-0 p-0">
        <div className="border-t p-2">
          <UserMenu />
        </div>
      </SidebarFooter>
    </Sidebar>
  )
}

export function ActiveNavigation() {
  return <Navigation pathname={usePathname()} />
}

export function Navigation({ pathname }: { pathname: string | null }) {
  const { setOpenMobile } = useSidebar()
  const isActive = (href: string) =>
    pathname !== null &&
    (href === "/" ? pathname === "/" || pathname.startsWith("/machines/") : pathname.startsWith(href))

  const item = ({ href, title, icon: Icon }: Section) => (
    <SidebarMenuItem key={href}>
      <SidebarMenuButton isActive={isActive(href)} render={<Link href={href} onClick={() => setOpenMobile(false)} />}>
        {/* Filled marks where you are, the way Tailscale and Linear do. */}
        <Icon weight={isActive(href) ? "fill" : "regular"} />
        <span>{title}</span>
      </SidebarMenuButton>
    </SidebarMenuItem>
  )
  return navigationGroups.map((group, index) => (
    <SidebarGroup key={group.label ?? index} className="px-2 py-1">
      {group.label && <SidebarGroupLabel>{group.label}</SidebarGroupLabel>}
      <SidebarMenu>{group.items.map(item)}</SidebarMenu>
    </SidebarGroup>
  ))
}
