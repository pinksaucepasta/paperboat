"use client"

import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { AudiencePicker } from "@/components/announcements"
import { SelectField, TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { useDebounced } from "@/hooks/use-debounced"
import { operatorAction, type Data } from "@/lib/api"

export type Notice = Data<"/v1/admin/notices">["notices"][number]

const kinds: [string, string][] = [
  ["price_increase", "Price increase (30 days' notice)"],
  ["terms_change", "Terms change (30 days' notice)"],
  ["subprocessor", "New subprocessor (30 days' notice)"],
  ["breach", "Data breach (DPA: within 48 hours)"],
]
const audiences: [string, string][] = [["all", "Everyone"], ["paid", "Owners of paid workspaces"], ["plan", "A plan"], ["team", "A team"], ["user", "One user"]]

function Long({ label, value, onChange, optional }: { label: string; value: string; onChange: (v: string) => void; optional?: boolean }) {
  const id = `notice-${label}`
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-baseline justify-between gap-2">
        <Label htmlFor={id}>{label}</Label>
        {optional && <span className="text-xs text-muted-foreground">Optional</span>}
      </div>
      <Textarea id={id} value={value} onChange={(e) => onChange(e.target.value)} maxLength={1000} rows={3} />
    </div>
  )
}

/**
 * Email a customer notice the legal pages promise: a price increase, a terms change, a new subprocessor or a breach.
 * The server renders the mail and counts the recipients (a dry run on every pause in typing), so what the operator
 * previews is what is sent and the server's refusal (a date under 30 days, a missing field) is the hint shown.
 */
export function NoticeDialog() {
  const [open, setOpen] = useState(false)
  const [f, setF] = useState({ kind: "price_increase", audience_kind: "paid", audience_value: "", effective: "", detail: "", link: "", urgent_reason: "", name: "", purpose: "", location: "", aware_at: "", what: "", data: "", actions: "", contact: "" })
  const set = (key: keyof typeof f) => (value: string) => setF((current) => ({ ...current, [key]: value }))
  const reset = () => setF({ ...f, effective: "", detail: "", link: "", urgent_reason: "", name: "", purpose: "", location: "", aware_at: "", what: "", data: "", actions: "", contact: "" })
  const awareIso = f.aware_at && !Number.isNaN(Date.parse(f.aware_at)) ? new Date(f.aware_at).toISOString() : ""
  const input = { ...f, aware_at: awareIso, audience_value: f.audience_kind === "all" || f.audience_kind === "paid" ? "" : f.audience_value }
  const settled = useDebounced(JSON.stringify(input), 600)
  const audienceReady = ["all", "paid"].includes(f.audience_kind) || f.audience_value !== ""
  const preview = useQuery({
    queryKey: ["notice-preview", settled],
    enabled: open && audienceReady,
    retry: false,
    queryFn: () => operatorAction<Notice>("POST", "/v1/admin/notices", { target: "notices", reason: "preview", key: crypto.randomUUID() }, { ...JSON.parse(settled), dry_run: true }),
  })
  const current = settled === JSON.stringify(input) && audienceReady ? preview.data : undefined
  const inSync = settled === JSON.stringify(input)
  const ready = current !== undefined && current.recipients > 0
  return (
    <ActionDialog
      open={open}
      onOpenChange={(next) => { setOpen(next); if (!next) reset() }}
      title="New customer notice"
      description="Emails the people you pick. Check the preview and the recipient count: a notice cannot be taken back."
      target="notices"
      submitLabel={current ? `Send to ${current.recipients}` : "Send"}
      valid={ready}
      trigger={(show) => <Button size="lg" onClick={show} data-action="new-notice">New notice</Button>}
      run={(h) => operatorAction<Notice>("POST", "/v1/admin/notices", h, input)}
    >
      <SelectField label="Notice" value={f.kind} onChange={set("kind")} options={kinds} />
      <AudiencePicker kind={f.audience_kind} value={f.audience_value} onKind={set("audience_kind")} onValue={set("audience_value")} options={audiences} />
      {f.kind !== "breach" && <TextField label="Takes effect on" type="date" value={f.effective} onChange={set("effective")} hint="At least 30 days from today." />}
      {f.kind === "price_increase" && <Long label="What changes" value={f.detail} onChange={set("detail")} />}
      {f.kind === "terms_change" && (
        <>
          <Long label="What changes" value={f.detail} onChange={set("detail")} />
          <TextField label="Link to the new text" value={f.link} onChange={set("link")} placeholder="https://…" />
          <TextField label="Why sooner than 30 days" optional value={f.urgent_reason} onChange={set("urgent_reason")} hint="Only for the law or a security problem." />
        </>
      )}
      {f.kind === "subprocessor" && (
        <>
          <TextField label="Subprocessor" value={f.name} onChange={set("name")} />
          <TextField label="What it will do" value={f.purpose} onChange={set("purpose")} placeholder="send transactional email" />
          <TextField label="Where" value={f.location} onChange={set("location")} placeholder="the United States" />
          <TextField label="Objections go to" value={f.contact} onChange={set("contact")} placeholder="privacy mailbox" />
        </>
      )}
      {f.kind === "breach" && (
        <>
          <TextField label="We became aware at" type="datetime-local" value={f.aware_at} onChange={set("aware_at")} hint="The DPA gives 48 hours from this moment." />
          <Long label="What happened" value={f.what} onChange={set("what")} />
          <Long label="Data involved" value={f.data} onChange={set("data")} />
          <Long label="What we are doing" value={f.actions} onChange={set("actions")} />
          <TextField label="Who to contact" value={f.contact} onChange={set("contact")} placeholder="privacy mailbox" />
        </>
      )}
      {f.kind !== "terms_change" && <TextField label="More information" optional value={f.link} onChange={set("link")} placeholder="https://…" />}
      <div className="flex flex-col gap-1.5" data-notice-preview>
        <Label>Preview</Label>
        {!audienceReady ? (
          <span className="text-sm text-muted-foreground">Choose who gets it.</span>
        ) : preview.isError && !current && inSync ? (
          <span className="text-sm text-destructive" role="status">{(preview.error as Error).message}</span>
        ) : current ? (
          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium">{current.subject}</span>
            <iframe title="Notice preview" sandbox="" srcDoc={current.html} className="h-48 w-full rounded-md border border-input bg-white" />
            <span className="text-xs text-muted-foreground">
              {current.recipients} recipient{current.recipients === 1 ? "" : "s"}
              {current.deadline && <> · 48-hour deadline <Time iso={current.deadline} /></>}
            </span>
            {current.late && <span className="text-xs text-destructive" role="alert">Past the 48-hour deadline. Send it now and the notice is recorded as late.</span>}
          </div>
        ) : (
          <span className="text-sm text-muted-foreground">Checking…</span>
        )}
      </div>
    </ActionDialog>
  )
}
