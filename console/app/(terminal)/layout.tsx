import { Suspense } from "react"
import { AnalyticsPageviews } from "@/components/analytics-pageviews"

/** Terminal pages take the whole tab, without the console's sidebar (see components/terminal/terminal-page.tsx). */
export default function TerminalLayout({ children }: { children: React.ReactNode }) {
  return (
    <>
      <Suspense>
        <AnalyticsPageviews />
      </Suspense>
      {children}
    </>
  )
}
