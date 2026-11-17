import { Suspense } from "react"
import { LoginForm } from "@/components/login-form"

export const metadata = { title: "Sign in" }

export default function LoginPage() {
  return (
    <div className="flex min-h-svh items-center justify-center p-6">
      <Suspense>
        <LoginForm />
      </Suspense>
    </div>
  )
}
