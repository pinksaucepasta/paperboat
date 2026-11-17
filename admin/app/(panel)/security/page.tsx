"use client"

import { useState } from "react"
import { CopyReference } from "@/components/copy-reference"
import { Page, Panel } from "@/components/page"
import { RecoveryCodes } from "@/components/recovery-codes"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { ApiError, post } from "@/lib/api"

export default function SecurityPage() {
  const [code, setCode] = useState("")
  const [codes, setCodes] = useState<string[]>()
  const [error, setError] = useState<Error>()
  const [busy, setBusy] = useState(false)
  return (
    <Page title="Security" description="Your sign-in uses a password and an authenticator app. Recovery codes get you in if you lose the authenticator.">
      <Panel title="Replace recovery codes">
        <form
          className="flex max-w-sm flex-col gap-2"
          onSubmit={async (e) => {
            e.preventDefault()
            setBusy(true)
            setError(undefined)
            try {
              setCodes((await post<{ recovery_codes: string[] }>("/v1/operator/mfa/recovery-codes", { code })).recovery_codes)
              setCode("")
            } catch (err) {
              setError(err as Error)
            } finally {
              setBusy(false)
            }
          }}
        >
          <p className="text-sm text-muted-foreground">Enter the current code from your authenticator. The old recovery codes stop working.</p>
          <Input aria-label="Authenticator code" inputMode="numeric" autoComplete="one-time-code" required value={code} onChange={(e) => setCode(e.target.value)} />
          {error && (
            <div role="alert" className="flex flex-col gap-1 text-sm text-destructive" data-security-error>
              <span>{error.message}</span>
              {error instanceof ApiError && error.reference && <CopyReference reference={error.reference} />}
            </div>
          )}
          <Button type="submit" disabled={busy}>Replace recovery codes</Button>
        </form>
        {codes && <RecoveryCodes codes={codes} />}
      </Panel>
    </Page>
  )
}
