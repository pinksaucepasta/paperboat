"use client"

import { CircleNotchIcon, XCircleIcon } from "@phosphor-icons/react"
import { useQuery } from "@tanstack/react-query"
import { useSearchParams } from "next/navigation"
import { useEffect, useRef } from "react"
import { api, ApiError } from "@/lib/api"
import { BrandScreen, Heading, Outcome, brandPrimary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"

/**
 * POST /v1/browser-access/issue: a one-use handoff for the preview or tunnel host. `denied`: the account
 * has no access; the handoff is posted all the same and the edge answers 403 with its own page.
 */
type Issue = { transaction_id: string; handoff: string; callback_origin: string; expires_at: string; denied?: boolean }

/**
 * Sign-in for a private or team preview or tunnel in the browser (F7-03). The edge sends a
 * signed-out browser here (/.paperboat/access/login?transaction_id=…, rewritten to this page); the
 * console session gate signs in first and returns here. The page asks the server for a handoff
 * and posts it to the preview host as a top-level form, so the edge can check the console's Origin
 * and its own cookie, then set its session. The handoff is never shown or logged. `switch=1` (the
 * edge's "Use a different account") signs this console session out first; the next sign-in returns to
 * this same address without `switch`.
 */
export function AccessLogin() {
  const params = useSearchParams()
  const transaction = params.get("transaction_id") ?? ""
  if (params.get("switch") === "1") {
    const rest = new URLSearchParams(params)
    rest.delete("switch")
    return <BrandScreen><SwitchAccount returnTo={`/.paperboat/access/login?${rest}`} /></BrandScreen>
  }
  return <BrandScreen>{transaction ? <Handoff transaction={transaction} /> : <Expired />}</BrandScreen>
}

function SwitchAccount({ returnTo }: { returnTo: string }) {
  const form = useRef<HTMLFormElement>(null)
  useEffect(() => form.current?.submit(), [])
  return (
    <form ref={form} method="post" action="/auth/sign-out" className="flex flex-col gap-6">
      <input type="hidden" name="return_to" value={returnTo} />
      <Heading title="Switching account…" text="Sign in with the account that should open this page." />
      <Button type="submit" size="lg" className={brandPrimary}>
        Continue
      </Button>
    </form>
  )
}

function Handoff({ transaction }: { transaction: string }) {
  const form = useRef<HTMLFormElement>(null)
  const issue = useQuery({
    queryKey: ["browser-access", transaction],
    queryFn: () =>
      api<Issue>("browser-access/issue", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ transaction_id: transaction }),
      }),
    retry: false,
    // One handoff per visit: never fetched again in the background (kept, not refetched, across the
    // development double mount).
    staleTime: Infinity,
  })
  const callback = issue.data && callbackUrl(issue.data.callback_origin)
  useEffect(() => {
    if (callback) form.current?.submit()
  }, [callback])

  if (issue.isError) return <Problem error={issue.error} />
  if (!issue.data) return <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
  if (!callback) return <Problem error={new Error("unexpected callback origin")} />
  return (
    <form ref={form} method="post" action={callback} className="flex flex-col gap-6">
      <input type="hidden" name="transaction_id" value={issue.data.transaction_id} />
      <input type="hidden" name="handoff" value={issue.data.handoff} />
      <Heading title="Signing you in…" text="Opening the page you asked for." />
      <Button type="submit" size="lg" className={brandPrimary}>
        Continue
      </Button>
    </form>
  )
}

/** The edge's callback on the preview or tunnel host; only an https origin is accepted. */
function callbackUrl(origin: string): string | undefined {
  try {
    const url = new URL(origin)
    if (url.protocol !== "https:" || url.pathname !== "/" || url.search || url.hash) return undefined
    return `${url.origin}/.paperboat/access/callback`
  } catch {
    return undefined
  }
}

function Problem({ error }: { error: Error }) {
  const status = error instanceof ApiError ? error.status : 0
  // An ended console session is already on its way to sign-in (confirmSession), back to this page.
  if (status === 401) return <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
  if (status === 404 || status === 410 || (error instanceof ApiError && /expired/.test(error.code))) return <Expired />
  return (
    <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="Couldn’t sign you in">
      Paperboat couldn’t sign you in right now. Try again.
    </Outcome>
  )
}

function Expired() {
  return (
    <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="This sign-in link expired">
      Open the preview or tunnel address again.
    </Outcome>
  )
}
