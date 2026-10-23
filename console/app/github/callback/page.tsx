import type { Metadata } from "next"
import { Suspense } from "react"
import { GitHubCallback } from "@/components/github/github-connect"

export const metadata: Metadata = { title: "Connecting GitHub" }

/** GitHub's return for a connect started in the console (the server's callback sends it here). */
export default function GitHubCallbackPage() {
  return (
    <Suspense>
      <GitHubCallback />
    </Suspense>
  )
}
