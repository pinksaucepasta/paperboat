import type { Metadata } from "next"
import { Suspense } from "react"
import { AccessLogin } from "@/components/access-login/access-login"

export const metadata: Metadata = { title: "Signing in" }

/** /.paperboat/access/login (the edge's login address on the console origin) is rewritten here. */
export default function AccessLoginPage() {
  return (
    <Suspense>
      <AccessLogin />
    </Suspense>
  )
}
