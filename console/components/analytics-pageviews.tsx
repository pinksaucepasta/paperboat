"use client"

import { useParams, usePathname } from "next/navigation"
import { useEffect } from "react"
import { optionalMeQuery, useApiQuery } from "@/lib/api"
import { pageleave, pageview, routePath } from "@/lib/analytics"

/** Signed-out visitors (guests on /terminal/{id}) send nothing and are not sent to sign-in. Sends the console's `$pageview` for each page (route template, no ids) and `$pageleave` when it is left. Renders nothing. */
export function AnalyticsPageviews() {
  const account = useApiQuery(optionalMeQuery).data?.id
  const pathname = usePathname()
  const params = useParams()
  const route = routePath(pathname, params)
  useEffect(() => {
    if (!account) return
    pageview(account, route, firstView ? document.referrer : undefined)
    firstView = false
    const leave = () => pageleave(account)
    window.addEventListener("pagehide", leave)
    return () => {
      window.removeEventListener("pagehide", leave)
      leave()
    }
  }, [account, route])
  return null
}

let firstView = true
