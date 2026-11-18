"use client"

import { useState } from "react"
import { BreakGlass } from "@/components/breakglass"
import { CopyReference } from "@/components/copy-reference"
import { Page, Panel } from "@/components/page"
import { RecoveryCodes } from "@/components/recovery-codes"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { ApiError, post } from "@/lib/api"

export default function SecurityPage() {
  const [code, setCode] = useState("")
  const [codes, setCodes] = useState<string[]>()
  const [error, setError] = useState<Error>()
  const [busy, setBusy] = useState(false)
  return (
    <Page title="Security">
      <Panel title="Recovery codes">
        <form
          className="flex max-w-sm flex-col gap-1.5"
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
          <Label htmlFor="mfa-code">Authenticator code</Label>
          <Input id="mfa-code" aria-label="Authenticator code" inputMode="numeric" autoComplete="one-time-code" required value={code} onChange={(e) => setCode(e.target.value)} />
          {error && (
            <div role="alert" className="flex flex-col gap-1 text-sm text-destructive" data-security-error>
              <span>{error.message}</span>
              {error instanceof ApiError && error.reference && <CopyReference reference={error.reference} />}
            </div>
          )}
          <span className="text-xs text-muted-foreground">Your current codes stop working.</span>
          <Button type="submit" size="lg" className="self-start" disabled={busy}>Replace recovery codes</Button>
        </form>
        {codes && <RecoveryCodes codes={codes} />}
      </Panel>
      <Panel title="Break glass">
        <BreakGlass />
      </Panel>
    </Page>
  )
}
