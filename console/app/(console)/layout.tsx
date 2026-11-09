import { Suspense } from "react"
import { ActiveNavigation, AppSidebar, Navigation } from "@/components/app-sidebar"
import { QuotaBanner } from "@/components/quota-banner"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { UserMenu } from "@/components/user-menu"
import { WithSession } from "@/components/with-session"

export default function ConsoleLayout({ children }: LayoutProps<"/">) {
  return (
    <SidebarProvider>
      <AppSidebar
        navigation={
          // The active item depends on the URL, which streams in after the static shell.
          <Suspense fallback={<Navigation pathname={null} />}>
            <ActiveNavigation />
          </Suspense>
        }
        userMenu={
          <WithSession>
            <UserMenu />
          </WithSession>
        }
      />
      <SidebarInset className="min-w-0">
        <QuotaBanner />
        <WithSession>{children}</WithSession>
      </SidebarInset>
    </SidebarProvider>
  )
}
