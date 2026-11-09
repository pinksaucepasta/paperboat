"use client"

import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ThemeProvider } from "next-themes"
import { useEffect, useState } from "react"
import { ApiError } from "@/lib/api"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"

export function Providers({ children }: { children: React.ReactNode }) {
  useEffect(() => {
    // The server writes dates in this zone (lib/server/prefetch.ts), so first paint matches the browser.
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
    if (!document.cookie.includes(`pb-tz=${encodeURIComponent(zone)}`)) {
      const secure = location.protocol === "https:" ? "; Secure" : ""
      document.cookie = `pb-tz=${encodeURIComponent(zone)}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`
    }
  }, [])
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            staleTime: 10_000,
            // Client errors will not change on retry; transient failures get two more tries.
            retry: (count, error) =>
              count < 2 && !(error instanceof ApiError && error.status >= 400 && error.status < 500),
          },
        },
      }),
  )
  return (
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          {children}
          <Toaster position="bottom-center" />
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>
  )
}
