import type { Metadata } from "next"
import { Suspense } from "react"
import { GitHubDone } from "@/components/github/github-connect"

export const metadata: Metadata = { title: "GitHub" }

/** GitHub's authorization ends here (the server redirects after completing it). */
export default function GitHubDonePage() {
  return (
    <Suspense>
      <GitHubDone />
    </Suspense>
  )
}
