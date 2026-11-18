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

type Step = "email" | "password" | "setup_password" | "setup_invited" | "verify" | "enroll" | "codes"
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
  const [token, setToken] = useState("")
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
        setStep(answer.next)
      } else if (step === "password" || step === "setup_password" || step === "setup_invited") {
        const session = await post<{ mfa: MfaState }>(
          step === "password" ? "/v1/operator/login" : "/v1/operator/setup",
          step === "setup_invited" ? { email, token: token.trim(), password } : { email, password },
        )
        setPassword("")
        setToken("")
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

  const choosing = step === "setup_password" || step === "setup_invited"
  const credentials = step === "email" || step === "password" || choosing
  return (
    <form onSubmit={submit} className="flex w-full max-w-sm flex-col gap-4" data-login-step={step}>
      <div className="flex flex-col gap-6">
        <div className="flex items-center gap-3">
          <Logo className="h-[18px]" />
          <span className="font-heading text-[18px] leading-none font-semibold tracking-[-0.025em]">paperboat</span>
        </div>
        <h1 className="font-heading text-2xl font-semibold tracking-tight">Admin sign-in</h1>
      </div>
      {credentials && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="email">Email</Label>
          <Input id="email" type="email" autoComplete="username" required value={email} disabled={(step !== "email" && step !== "setup_invited") || busy} onChange={(e) => setEmail(e.target.value)} />
        </div>
      )}
      {step === "setup_invited" && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="token">Setup token</Label>
          <Input id="token" autoComplete="off" required autoFocus value={token} onChange={(e) => setToken(e.target.value)} />
        </div>
      )}
      {(step === "password" || choosing) && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="password">{choosing ? "New password" : "Password"}</Label>
          <Input
            id="password"
            type="password"
            autoComplete={choosing ? "new-password" : "current-password"}
            required
            minLength={choosing ? 12 : undefined}
            autoFocus={step !== "setup_invited"}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          {choosing && <span className="text-xs text-muted-foreground">12 or more characters.</span>}
        </div>
      )}
      {step === "enroll" && enrollment && (
        <div className="flex flex-col gap-3" data-mfa-enroll>
          <p className="text-sm text-muted-foreground">Scan the code with an authenticator app, or type the key.</p>
          {qr && (
            // eslint-disable-next-line @next/next/no-img-element -- a generated data: URL, nothing to optimise
            <img src={qr} alt="Authenticator QR code" width={192} height={192} className="self-center rounded-md bg-white p-1" />
          )}
          <code className="surface break-all p-3 font-mono text-xs" data-mfa-secret>{enrollment.secret}</code>
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
      {(step === "email" || step === "setup_invited") && (
        <Button type="button" variant="link" size="sm" className="self-start px-0" onClick={() => setStep(step === "email" ? "setup_invited" : "email")}>
          {step === "email" ? "I have a setup token" : "Back to sign-in"}
        </Button>
      )}
      <Button type="submit" size="lg" disabled={busy}>
        {step === "email" ? "Continue" : choosing ? "Set password and continue" : step === "password" ? "Continue" : step === "codes" ? "I saved them, open the panel" : step === "enroll" ? "Confirm and finish" : "Sign in"}
      </Button>
    </form>
  )
}
