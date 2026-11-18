import type { Metadata } from "next"
import { cookies } from "next/headers"
import { redirect } from "next/navigation"
import { Suspense } from "react"
import { BrandScreen, brandPrimary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"
import { controlPlaneFetch, sessionCookieNames } from "@/lib/server/control-plane"

export const metadata: Metadata = { title: "Sign-in failed" }

const refusals: Record<string, { title: string; text: string }> = {
  account_banned: { title: "This account can’t sign in", text: "Paperboat has disabled this account. If you think this is a mistake, contact support and quote the reference." },
  ip_blocked: { title: "Sign-in isn’t available from this network", text: "Paperboat blocked sign-in from this network. Try another network, or contact support and quote the reference." },
}

type Search = Promise<{ reason?: string; ref?: string }>

export default function SignInFailedPage({ searchParams }: { searchParams: Search }) {
  return (
    <BrandScreen>
      <Suspense>
        <LeaveWhenSignedIn />
      </Suspense>
      {/* The reason is in the address, which a static page cannot read: it streams in, and until it does nothing is said. */}
      <Suspense fallback={null}>
        <Failure searchParams={searchParams} />
      </Suspense>
    </BrandScreen>
  )
}

async function Failure({ searchParams }: { searchParams: Search }) {
  const { reason, ref } = await searchParams
  const refusal = reason ? refusals[reason] : undefined
  return (
    <div className="flex flex-col items-center gap-3 text-center">
      <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em]">
        {refusal ? refusal.title : "We couldn’t sign you in"}
      </h1>
      <p className="max-w-md text-base text-white/75">{refusal ? refusal.text : "Nothing changed. Try again."}</p>
      {refusal ? (
        ref && /^[\w.-]{1,80}$/.test(ref) && <code className="rounded-md bg-white/10 px-2 py-1 text-sm text-white/85">{ref}</code>
      ) : (
        <Button size="lg" className={`mt-3 ${brandPrimary}`} render={<a href="/auth/sign-in" />} nativeButton={false}>
          Sign in
        </Button>
      )}
    </div>
  )
}

/** Someone already signed in (a stale tab, the back button) goes to the console instead. */
async function LeaveWhenSignedIn() {
  const jar = await cookies()
  if (!sessionCookieNames.some((name) => jar.has(name))) return null
  const me = await controlPlaneFetch("/v1/me", { headers: { cookie: jar.toString() } }).catch(() => null)
  if (me?.ok) redirect("/")
  return null
}
