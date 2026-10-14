"use client"

import { DotsThreeVerticalIcon, MonitorIcon, MoonIcon, SignOutIcon, SunIcon, UserIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useTheme } from "next-themes"
import { useRef } from "react"
import { useApiQuery, meQuery } from "@/lib/api"
import { useHideEmail } from "@/lib/privacy"
import { Email } from "@/components/email"
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
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem } from "@/components/ui/sidebar"
import { Skeleton } from "@/components/ui/skeleton"

const themes = [
  { value: "system", label: "System", icon: MonitorIcon },
  { value: "light", label: "Light", icon: SunIcon },
  { value: "dark", label: "Dark", icon: MoonIcon },
] as const

export function UserMenu() {
  const { data: me } = useApiQuery(meQuery)
  const { theme, setTheme } = useTheme()
  const ThemeIcon = themes.find((item) => item.value === theme)?.icon ?? MonitorIcon
  const signOut = useRef<HTMLFormElement>(null)
  const hideEmail = useHideEmail()
  // Initials from the email would leak part of it while it is hidden.
  const initialsSource = me?.display_name || (hideEmail ? "" : me?.email) || ""

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger render={<SidebarMenuButton size="lg" />}>
            <Avatar className="size-8">
              <AvatarFallback className="bg-primary text-xs font-medium text-primary-foreground">
                {initialsSource ? initials(initialsSource) : <UserIcon weight="bold" className="size-4" />}
              </AvatarFallback>
            </Avatar>
            {me ? (
              <span className="flex min-w-0 flex-1 flex-col text-left leading-tight">
                {me.display_name ? (
                  <>
                    <span className="truncate font-medium">{me.display_name}</span>
                    <Email value={me.email} className="truncate text-xs text-muted-foreground" />
                  </>
                ) : (
                  <Email value={me.email} className="truncate font-medium" />
                )}
              </span>
            ) : (
              <Skeleton className="h-8 flex-1" />
            )}
            <DotsThreeVerticalIcon weight="bold" className="text-muted-foreground" />
          </DropdownMenuTrigger>
          <DropdownMenuContent side="top" align="start" className="w-(--anchor-width) min-w-56">
            <DropdownMenuItem render={<Link href="/account" />}>
              <UserIcon /> Account
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            {/* shadcn's pattern for themes: radio items (here in a submenu), not a toggle group. */}
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
            <DropdownMenuItem onClick={() => signOut.current?.requestSubmit()}>
              <SignOutIcon /> Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        <form ref={signOut} action="/auth/sign-out" method="post" hidden />
      </SidebarMenuItem>
    </SidebarMenu>
  )
}

function initials(name: string): string {
  const words = name.split(/[\s@._-]+/).filter(Boolean)
  return ((words[0]?.[0] ?? "") + (words[1]?.[0] ?? "")).toUpperCase() || "·"
}
