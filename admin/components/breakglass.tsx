"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { TextField } from "@/components/fields"
import { StatusBadge } from "@/components/filters"
import { Facts } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { ApiError, operatorAction, useApiQuery, type Data } from "@/lib/api"

type Preview = Data<"/v1/admin/breakglass/preview">
type Run = Data<"/v1/admin/breakglass/runs/{run_id}">
type Stage = Run["stages"][number]

/** The phrase the server wants typed; the dialog asks for it before it enables the button. */
const phrase = "BREAK GLASS"

const stageTitles: Record<Stage["name"], [string, string]> = {
  publish: ["Publish next keys", "Add the next SSH CA beside the active one for every account. Machines trust both."],
  pickup: ["Wait for machines", "Until online machines fetched the next keys, or the timeout."],
  switch: ["Switch signing", "The next CAs sign; the old ones stay trusted while certificates run out."],
  revoke: ["Sign everyone out", "Every browser, CLI, API token and recipe session ends, and unused enrollment tokens are cancelled, except your own session."],
  reseal: ["Re-seal secrets", "Stored secrets move to the newest encryption key."],
  floor: ["Raise the update floor", "Clients older than the floor are told to update."],
  retire: ["Retire old keys", "The old CAs are removed at once. The old mint keys are withdrawn about an hour after the run starts, once machine credentials signed by them have expired."],
}

const counts = (detail: Record<string, unknown>) =>
  Object.entries(detail)
    .filter(([key, value]) => typeof value === "number" && !key.startsWith("skip:"))
    .map(([key, value]) => `${key.replaceAll("_", " ")} ${value}`)
    .join(" · ")

/** Break-glass after a server breach: the preview, the guarded start and the live run. */
export function BreakGlass() {
  const preview = useApiQuery<Preview>(["breakglass", "preview"], "/v1/admin/breakglass/preview")
  const [watching, setWatching] = useState(false)
  const latest = useApiQuery<Run>(["breakglass", "run"], "/v1/admin/breakglass/runs/latest", watching ? 2000 : undefined)
  const run = latest.data
  const none = latest.error instanceof ApiError && latest.error.status === 404
  if (run && run.state === "running" && !watching) setWatching(true)
  if (run && run.state !== "running" && watching) setWatching(false)
  if (latest.isPending) return <Skeleton className="h-40 w-full" />
  if (latest.error && !none) return <ErrorBlock what="the latest run" error={latest.error} retry={() => latest.refetch()} />
  return (
    <div className="flex flex-col gap-6" data-breakglass>
      {run && <RunView run={run} />}
      {(!run || run.state !== "running") && (preview.error ? <ErrorBlock what="the preview" error={preview.error} retry={() => preview.refetch()} /> : preview.data ? <PreviewView preview={preview.data} onStarted={() => setWatching(true)} /> : <Skeleton className="h-40 w-full" />)}
    </div>
  )
}

function RunView({ run }: { run: Run }) {
  const waiting = run.stages.find((s) => s.state === "running" && run.pickups?.[s.name])
  const pickup = waiting ? run.pickups?.[waiting.name] : undefined
  return (
    <div className="flex flex-col gap-4" data-breakglass-run={run.state}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="flex items-center gap-3 text-sm">
          <StatusBadge state={run.state} />
          <span className="text-muted-foreground">Started <Time iso={run.started_at} relative /> · {run.reason}</span>
        </span>
        {run.state === "running" && <Cancel id={run.id} />}
      </div>
      <ol className="flex flex-col divide-y rounded-lg border">
        {run.stages.map((stage, index) => (
          <li key={stage.name} className="flex flex-col gap-1 px-4 py-3" data-stage={stage.name} data-stage-state={stage.state}>
            <div className="flex items-center justify-between gap-3">
              <span className="text-sm font-medium">{index + 1}. {stageTitles[stage.name][0]}</span>
              <StatusBadge state={stage.state} />
            </div>
            <span className="text-xs text-muted-foreground">{stageTitles[stage.name][1]}</span>
            {stage.state !== "pending" && stage.state !== "skipped" && <span className="font-mono text-xs">{counts(stage.detail)}</span>}
            {stage.name === "retire" && typeof stage.detail.mint_withdraw_at === "string" && stage.state !== "done" && (
              <span className="text-xs" data-mint-countdown>Old mint keys are withdrawn <Time iso={stage.detail.mint_withdraw_at} relative /> (machines renew their credentials first)</span>
            )}
          </li>
        ))}
      </ol>
      {run.last_error && <div role="alert" className="text-sm text-destructive">{run.last_error} The worker retries it.</div>}
      {pickup && waiting && (
        <div className="flex flex-col gap-2" data-pickup>
          <span className="text-sm font-medium">{pickup.picked_up} of {pickup.expected} online machines hold the new keys</span>
          <ul className="flex flex-col divide-y rounded-lg border text-sm">
            {pickup.machines.map((m) => (
              <li key={m.machine_id} className="flex items-center justify-between gap-3 px-4 py-2">
                <span className="font-mono text-xs">{m.machine_id} <span className="text-muted-foreground">{m.platform}</span></span>
                <span className="flex items-center gap-3"><StatusBadge state={m.picked_up ? "done" : "pending"} /><Time iso={m.fetched_at} relative /></span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {run.state === "done" && <Remaining preview={run.preview} />}
    </div>
  )
}

/** What the server cannot do for itself once the run is done: drop the old keys from its configuration. */
function Remaining({ preview }: { preview: Preview }) {
  return (
    <div className="flex flex-col gap-2 rounded-lg border p-4 text-sm" data-breakglass-remaining>
      <span className="font-medium">Still to do by hand</span>
      <ul className="list-disc pl-5 text-muted-foreground">
        <li>Remove the old mint key, session key and encryption key from the server configuration and deploy ({preview.encryption_key_ids.length} encryption keys, {preview.session_key_ids.length} session keys configured now).</li>
        <li>Rotate the third-party credentials below.</li>
        <li>Tell affected customers and, within 72 hours of learning of a breach, the supervisory authority.</li>
      </ul>
    </div>
  )
}

function Cancel({ id }: { id: string }) {
  return (
    <ActionDialog
      title="Cancel the run"
      description="Stops after the current step. What was done stays done; a new run finishes the rest."
      target={id}
      destructive
      submitLabel="Cancel run"
      trigger={(open) => <Button variant="outline" onClick={open}>Cancel run</Button>}
      run={(h) => operatorAction("POST", `/v1/admin/breakglass/runs/${id}/cancel`, h)}
    />
  )
}

function PreviewView({ preview, onStarted }: { preview: Preview; onStarted: () => void }) {
  const ready = preview.checks.every((c) => c.ok)
  const total = (m: Record<string, number>) => Object.values(m).reduce((a, b) => a + b, 0)
  return (
    <div className="flex flex-col gap-5" data-breakglass-preview>
      <p className="max-w-prose text-sm text-muted-foreground">
        After a server breach, deploy the new keys first (mint signing key as active beside the old one, a new session key first, <code>PAPERBOAT_ENCRYPTION_KEY</code> as <code>new,old</code>). Then this action rotates the SSH CAs, signs everyone out, re-seals stored secrets and retires the old keys. Machines are not reinstalled.
      </p>
      <ul className="flex flex-col divide-y rounded-lg border text-sm" data-breakglass-checks>
        {preview.checks.map((c) => (
          <li key={c.name} className="flex items-center justify-between gap-3 px-4 py-2" data-check={c.name} data-check-ok={c.ok}>
            <span><span className="font-medium">{c.name}</span> <span className="text-muted-foreground">{c.detail}</span></span>
            <StatusBadge state={c.ok ? "ready" : "blocked"} />
          </li>
        ))}
      </ul>
      <Facts
        rows={[
          ["Accounts with an SSH CA", `${preview.accounts_with_ssh_ca} (${preview.ssh_cas} CAs${preview.accounts_mid_rotation ? `, ${preview.accounts_mid_rotation} mid-rotation are left alone` : ""})`],
          ["Machines expected to pick up the new keys", `${preview.machines_expected_to_pick_up} online of ${preview.machines}`],
          ["Sessions and tokens revoked", `${total(preview.sessions_and_tokens_to_revoke)} (${Object.entries(preview.sessions_and_tokens_to_revoke).map(([k, v]) => `${k.replaceAll("_", " ")} ${v}`).join(", ")})`],
          ["Other operator sessions revoked", String(preview.other_operator_sessions_to_revoke)],
          ["Stored secrets re-sealed", String(total(preview.sealed_rows_to_reseal))],
          ["Mint keys withdrawn", preview.mint_keys_to_withdraw.length ? `${preview.mint_keys_to_withdraw.join(", ")}, ${preview.mint_withdraw_after_minutes} minutes after the run starts (machines renew their credentials first)` : "none"],
          ["Client update floor now", preview.client_update_floor_now || "none"],
        ]}
      />
      <Start ready={ready} onStarted={onStarted} />
      <div className="flex flex-col gap-2" data-breakglass-thirdparty>
        <span className="text-sm font-medium">Third-party credentials: rotate each at the provider, then deploy the new value</span>
        <ul className="flex flex-col divide-y rounded-lg border text-sm">
          {preview.third_party_checklist.map((p) => (
            <li key={p.name} className="flex flex-wrap items-center justify-between gap-2 px-4 py-2">
              <span><span className="font-medium">{p.name}</span> <span className="text-muted-foreground">{p.what}. {p.where}</span></span>
              {p.link && <a className="text-xs underline underline-offset-4" href={p.link} target="_blank" rel="noreferrer">Open</a>}
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}

function Start({ ready, onStarted }: { ready: boolean; onStarted: () => void }) {
  const [reason, setReason] = useState("")
  const [code, setCode] = useState("")
  const [floor, setFloor] = useState("")
  return (
    <div className="flex items-center gap-3">
      <ActionDialog
        title="Break glass"
        description="Rotates the SSH CAs, signs every session and token out except yours, re-seals stored secrets and retires the old keys. It runs in stages you can watch, and cannot be undone."
        target="breakglass"
        destructive
        confirmWord={phrase}
        submitLabel="Break glass"
        valid={reason.trim().length >= 8 && /^\d{6}$/.test(code.replace(/\s/g, ""))}
        trigger={(open) => <Button variant="destructive" size="lg" disabled={!ready} onClick={open}>Break glass…</Button>}
        run={(h) => operatorAction("POST", "/v1/admin/breakglass/runs", h, { phrase, reason: reason.trim(), totp_code: code.replace(/\s/g, ""), floor_version: floor.trim() || undefined })}
        onDone={() => { setReason(""); setCode(""); setFloor(""); onStarted() }}
      >
        <div className="flex flex-col gap-1.5">
          <label className="text-xs font-medium" htmlFor="bg-reason">Why (kept with the audit trail)</label>
          <Textarea id="bg-reason" value={reason} onChange={(e) => setReason(e.target.value)} maxLength={500} rows={3} />
        </div>
        <TextField label="Authenticator code" value={code} onChange={setCode} inputMode="numeric" autoComplete="one-time-code" hint="Checked when you confirm: a current code, used once." />
        <TextField label="Raise the client update floor to" value={floor} onChange={setFloor} optional hint="A release version such as 2026.11.18.1; older clients are told to update." />
      </ActionDialog>
      {!ready && <span className="text-sm text-muted-foreground">Deploy the missing keys above first.</span>}
    </div>
  )
}
