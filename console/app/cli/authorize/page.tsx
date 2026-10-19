import type { Metadata } from "next"
import { Suspense } from "react"
import { CliAuthorize } from "@/components/cli/cli-authorize"

export const metadata: Metadata = { title: "Add this computer" }

/** `pb login` and the installer open this page (the server's CLI verification URL) with `?code=`. */
export default function CliAuthorizePage() {
  return (
    <Suspense>
      <CliAuthorize />
    </Suspense>
  )
}
