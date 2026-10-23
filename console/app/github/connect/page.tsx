import type { Metadata } from "next"
import { Suspense } from "react"
import { GitHubConnect } from "@/components/github/github-connect"

export const metadata: Metadata = { title: "Connect GitHub" }

/** `pb` opens this page (from the server's native-link response) to connect GitHub. */
export default function GitHubConnectPage() {
  return (
    <Suspense>
      <GitHubConnect />
    </Suspense>
  )
}
