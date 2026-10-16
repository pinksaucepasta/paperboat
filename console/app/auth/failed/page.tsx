import type { Metadata } from "next"
import { cookies } from "next/headers"
import { redirect } from "next/navigation"
import { Suspense } from "react"
import { BrandScreen, brandPrimary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"
import { controlPlaneFetch, sessionCookieNames } from "@/lib/server/control-plane"

export const metadata: Metadata = { title: "Sign-in failed" }

const reasons: Record<string, { title: string; detail: string }> = {
  email_alias_not_allowed: {
    title: "Use your main email address",
    detail: "Addresses with a + in them can’t be used for a Paperboat account. Sign in with your main address.",
  },
  email_already_registered: {
    title: "You already have an account",
    detail: "This email belongs to an existing Paperboat account. Sign in with the address you used before.",
  },
}

export default async function SignInFailedPage({ searchParams }: { searchParams: Promise<{ reason?: string }> }) {
  const { reason } = await searchParams
  const copy = (reason && reasons[reason]) || { title: "We couldn’t sign you in", detail: "Nothing changed. Try again." }
  return (
    <BrandScreen>
      <Suspense>
        <LeaveWhenSignedIn />
      </Suspense>
      <div className="flex flex-col items-center gap-3 text-center">
        <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em]">
          {copy.title}
        </h1>
        <p className="max-w-sm text-base text-white/75">{copy.detail}</p>
        <Button size="lg" className={`mt-3 ${brandPrimary}`} render={<a href="/auth/sign-in" />} nativeButton={false}>
          Sign in
        </Button>
      </div>
    </BrandScreen>
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
