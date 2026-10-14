import type { Metadata } from "next"
import { Suspense } from "react"
import { CliAuthorize } from "@/components/cli/cli-authorize"

export const metadata: Metadata = { title: "Approve CLI sign-in" }

/** `pb login` opens this page (the server's CLI verification URL) with `?code=`. */
export default function CliAuthorizePage() {
  return (
    <Suspense>
      <CliAuthorize />
    </Suspense>
  )
}
