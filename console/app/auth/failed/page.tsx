import type { Metadata } from "next"
import { Logo } from "@/components/logo"
import { Button } from "@/components/ui/button"

export const metadata: Metadata = { title: "Sign-in failed" }

export default function SignInFailedPage() {
  return (
    <main className="flex min-h-svh flex-col items-center justify-center gap-6 px-6 text-center">
      <Logo className="h-6" />
      <div className="flex flex-col gap-1.5">
        <h1 className="font-heading text-lg font-semibold">We couldn’t sign you in</h1>
        <p className="text-sm text-muted-foreground">Nothing changed. Try again.</p>
      </div>
      <Button size="lg" render={<a href="/auth/sign-in" />}>
        Sign in
      </Button>
    </main>
  )
}
