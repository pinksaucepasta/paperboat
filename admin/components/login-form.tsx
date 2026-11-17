"use client"

import QRCode from "qrcode"
import { useRouter, useSearchParams } from "next/navigation"
import { useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { CopyReference } from "@/components/copy-reference"
import { RecoveryCodes } from "@/components/recovery-codes"
import { ApiError, api, post, type MfaState, type Operator } from "@/lib/api"
import { Logo } from "@/components/logo"

/** Only an in-panel absolute path may follow sign-in, never another origin. */
function safeReturn(value: string | null): string {
  return value && value.startsWith("/") && !value.startsWith("//") && !/[\\\s]/.test(value) && !value.startsWith("/login") ? value : "/"
}

type Step = "email" | "password" | "setup_password" | "verify" | "enroll" | "codes"
type Enrollment = { secret: string; otpauth_uri: string }

/**
 * Operator sign-in: email, password (or first-time password setup), then the second factor: a code from the
 * authenticator, or on the first sign-in a short enrolment (scan, confirm, save recovery codes).
 */
export function LoginForm() {
  const router = useRouter()
  const returnTo = safeReturn(useSearchParams().get("return_to"))
  const [step, setStep] = useState<Step>("email")
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [code, setCode] = useState("")
  const [enrollment, setEnrollment] = useState<Enrollment>()
  const [qr, setQr] = useState<string>()
  const [recovery, setRecovery] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | Error>()

  // A reload mid-sign-in keeps its session: resume at the step it still needs.
  useEffect(() => {
    api<Operator>("/v1/operator/me")
      .then((me) => {
        if (me.mfa === "verified") router.replace(returnTo)
        else {
          setEmail(me.email)
          void startMfa(me.mfa)
        }
      })
      .catch(() => {})
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once, on arrival
  }, [])

  useEffect(() => {
    if (!enrollment) return
    QRCode.toDataURL(enrollment.otpauth_uri, { margin: 1, width: 192 }).then(setQr, () => setQr(undefined))
  }, [enrollment])

  async function startMfa(state: MfaState) {
    if (state === "verify") return setStep("verify")
    setEnrollment(await post<Enrollment>("/v1/operator/mfa/enroll"))
    setStep("enroll")
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      if (step === "email") {
        const answer = await post<{ next: "password" | "setup_password" }>("/v1/operator/email", { email })
        setStep(answer.next === "setup_password" ? "setup_password" : "password")
      } else if (step === "password" || step === "setup_password") {
        const session = await post<{ mfa: MfaState }>(step === "password" ? "/v1/operator/login" : "/v1/operator/setup", { email, password })
        setPassword("")
        await startMfa(session.mfa)
      } else if (step === "verify") {
        await post("/v1/operator/mfa/verify", { code })
        router.replace(returnTo)
      } else if (step === "enroll") {
        const done = await post<{ recovery_codes: string[] }>("/v1/operator/mfa/enroll/confirm", { code })
        setRecovery(done.recovery_codes)
        setStep("codes")
      } else {
        router.replace(returnTo)
      }
      setCode("")
    } catch (e) {
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }

  const credentials = step === "email" || step === "password" || step === "setup_password"
  return (
    <form onSubmit={submit} className="flex w-full max-w-sm flex-col gap-4" data-login-step={step}>
      <div className="flex items-center gap-2">
        <Logo className="size-6" />
        <h1 className="text-lg font-medium">Paperboat admin</h1>
      </div>
      <p className="text-sm text-muted-foreground">Operator sign-in. Customer accounts cannot sign in here.</p>
      {credentials && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="email">Email</Label>
          <Input id="email" type="email" autoComplete="username" required value={email} disabled={step !== "email" || busy} onChange={(e) => setEmail(e.target.value)} />
        </div>
      )}
      {(step === "password" || step === "setup_password") && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="password">{step === "setup_password" ? "Choose a password (12 or more characters)" : "Password"}</Label>
          <Input
            id="password"
            type="password"
            autoComplete={step === "setup_password" ? "new-password" : "current-password"}
            required
            minLength={step === "setup_password" ? 12 : undefined}
            autoFocus
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>
      )}
      {step === "enroll" && enrollment && (
        <div className="flex flex-col gap-3" data-mfa-enroll>
          <p className="text-sm">Add Paperboat to an authenticator app (1Password, Authy, Google Authenticator…): scan the code or type the key. Then enter the 6-digit code it shows.</p>
          {qr && (
            // eslint-disable-next-line @next/next/no-img-element -- a generated data: URL, nothing to optimise
            <img src={qr} alt="Authenticator QR code" width={192} height={192} className="self-center rounded-md bg-white p-1" />
          )}
          <code className="break-all rounded-md border p-2 text-xs" data-mfa-secret>{enrollment.secret}</code>
        </div>
      )}
      {(step === "verify" || step === "enroll") && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="code">{step === "verify" ? "Authenticator code or recovery code" : "6-digit code"}</Label>
          <Input id="code" inputMode={step === "enroll" ? "numeric" : "text"} autoComplete="one-time-code" required autoFocus value={code} onChange={(e) => setCode(e.target.value)} />
        </div>
      )}
      {step === "codes" && <RecoveryCodes codes={recovery} />}
      {error && (
        <div role="alert" className="flex flex-col gap-1 text-sm text-destructive" data-login-error>
          <span>{error instanceof ApiError && error.status === 401 && credentials ? "Sign-in failed. Check the email and password." : error.message}</span>
          {error instanceof ApiError && error.reference && <CopyReference reference={error.reference} />}
        </div>
      )}
      <Button type="submit" disabled={busy}>
        {step === "email" ? "Continue" : step === "setup_password" ? "Set password and continue" : step === "password" ? "Continue" : step === "codes" ? "I saved them, open the panel" : step === "enroll" ? "Confirm and finish" : "Sign in"}
      </Button>
    </form>
  )
}
