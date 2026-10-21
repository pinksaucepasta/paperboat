import { Suspense } from "react"
import { ActiveNavigation, AppSidebar, Navigation } from "@/components/app-sidebar"
import { QuotaBanner } from "@/components/quota-banner"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"

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
      />
      <SidebarInset className="min-w-0">
        <QuotaBanner />
        {children}
      </SidebarInset>
    </SidebarProvider>
  )
}
