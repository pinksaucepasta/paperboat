import type { Metadata } from "next"
import { BrandScreen, brandPrimary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"

export const metadata: Metadata = { title: "Sign-in failed" }

export default function SignInFailedPage() {
  return (
    <BrandScreen>
      <div className="flex flex-col items-center gap-3 text-center">
        <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em]">
          We couldn’t sign you in
        </h1>
        <p className="text-base text-white/75">Nothing changed. Try again.</p>
        <Button size="lg" className={`mt-3 ${brandPrimary}`} render={<a href="/auth/sign-in" />} nativeButton={false}>
          Sign in
        </Button>
      </div>
    </BrandScreen>
  )
}
