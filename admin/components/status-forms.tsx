"use client"

import { useRef, useState } from "react"
import { Panel } from "@/components/page"
import { SelectField, TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { ApiError, send } from "@/lib/api"

export type Incident = { id: string; title: string; status: string; components: string[]; created: number; updates: { status: string; body: string; ts: number }[] }
export type Maint = { id: string; title: string; components: string[]; starts: number; ends: number }
export type Snapshot = {
  public: { components: { id: string; name: string; state: string; units: { id: string; name: string; state: string }[] }[]; incidents: Incident[]; maintenance: Maint[] }
  admin: { evidence: Record<string, { vantage: string; view: string; last?: { ms: number } }[]>; vantages: { vantage: string; last: number | null; net_ok: boolean | null; build: string }[]; subscribers: number }
}

const statuses: [string, string][] = [["investigating", "Investigating"], ["identified", "Identified"], ["monitoring", "Monitoring"], ["resolved", "Resolved"]]
const iso = (ms: number) => new Date(ms).toISOString()

/** A form dialog: runs `send`, shows the failure plainly, closes and refreshes on success. */
function FormDialog({ title, submit, valid, run: send, onDone, children, trigger }: { title: string; submit: string; valid: boolean; run: (key: string) => Promise<unknown>; onDone: () => void; children: React.ReactNode; trigger: string }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const key = useRef("") // one idempotency key per opening: a retried publish cannot create a second incident or mail twice
  async function run() {
    setBusy(true); setError(null)
    try {
      const result = (await send(key.current)) as { mail?: { failed: number; truncated?: boolean } } | undefined
      if (result?.mail?.failed || result?.mail?.truncated) { setError(`Saved, but ${result.mail.failed} email(s) could not be sent${result.mail.truncated ? " and the subscriber list was cut off" : ""}.`); onDone(); return }
      setOpen(false); onDone()
    } catch (e) { setError(e instanceof ApiError ? e.message : "The request failed.") } finally { setBusy(false) }
  }
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => { key.current = crypto.randomUUID(); setError(null); setOpen(true) }}>{trigger}</Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{title}</DialogTitle></DialogHeader>
          <div className="flex flex-col gap-4">{children}</div>
          {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button disabled={!valid || busy} onClick={run}>{submit}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

function Components({ s, value, onChange }: { s: Snapshot; value: string[]; onChange: (v: string[]) => void }) {
  return (
    <fieldset className="flex flex-col gap-1.5">
      <legend className="mb-1 text-xs font-medium">Affected components</legend>
      {s.public.components.map((c) => (
        <Label key={c.id} className="flex items-center gap-2 font-normal">
          <input type="checkbox" checked={value.includes(c.id)} onChange={(e) => onChange(e.target.checked ? [...value, c.id] : value.filter((x) => x !== c.id))} />
          {c.name}
        </Label>
      ))}
    </fieldset>
  )
}

function Notes({ value, onChange, label }: { value: string; onChange: (v: string) => void; label: string }) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label>{label}</Label>
      <Textarea value={value} onChange={(e) => onChange(e.target.value)} rows={4} maxLength={4000} />
    </div>
  )
}

function NewIncident({ s, refresh }: { s: Snapshot; refresh: () => void }) {
  const [title, setTitle] = useState("")
  const [body, setBody] = useState("")
  const [status, setStatus] = useState("investigating")
  const [components, setComponents] = useState<string[]>([])
  return (
    <FormDialog title="New incident" trigger="New incident" submit="Publish" valid={!!title.trim() && !!body.trim() && components.length > 0}
      run={(key) => send("POST", "/status/incidents", { title, body, status, components }, { "idempotency-key": key })} onDone={() => { setTitle(""); setBody(""); setComponents([]); refresh() }}>
      <TextField label="Title" value={title} onChange={setTitle} maxLength={140} />
      <SelectField label="Status" value={status} onChange={setStatus} options={statuses} />
      <Components s={s} value={components} onChange={setComponents} />
      <Notes label="What users should know" value={body} onChange={setBody} />
    </FormDialog>
  )
}

function IncidentUpdate({ i, refresh }: { i: Incident; refresh: () => void }) {
  const [status, setStatus] = useState(i.status === "resolved" ? "resolved" : i.status)
  const [body, setBody] = useState("")
  return (
    <FormDialog title={`Update: ${i.title}`} trigger="Post update" submit="Publish" valid={!!body.trim()} run={(key) => send("POST", `/status/incidents/${i.id}/updates`, { status, body }, { "idempotency-key": key })} onDone={() => { setBody(""); refresh() }}>
      <SelectField label="Status" value={status} onChange={setStatus} options={statuses} />
      <Notes label="Update" value={body} onChange={setBody} />
    </FormDialog>
  )
}

export function StatusIncidents({ s, refresh }: { s: Snapshot; refresh: () => void }) {
  const names = new Map(s.public.components.map((c) => [c.id, c.name]))
  return (
    <Panel title="Incidents" action={<NewIncident s={s} refresh={refresh} />}>
      {s.public.incidents.length === 0 ? (
        <p className="text-sm text-muted-foreground">No incidents in the last 14 days.</p>
      ) : (
        <div className="divide-y divide-divider border-y border-divider text-sm">
          {s.public.incidents.map((i) => (
            <div key={i.id} className="flex flex-col gap-1 py-3" data-status-incident={i.id}>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="font-medium">{i.title}</span>
                {i.status !== "resolved" && <IncidentUpdate i={i} refresh={refresh} />}
              </div>
              <span className="text-xs text-muted-foreground">{statuses.find(([v]) => v === i.status)?.[1]} · {i.components.map((c) => names.get(c) ?? c).join(", ")}</span>
              {i.updates.map((u) => (
                <p key={u.ts} className="text-xs"><b className="capitalize">{u.status}</b> {u.body} <span className="text-muted-foreground"><Time iso={iso(u.ts)} relative /></span></p>
              ))}
            </div>
          ))}
        </div>
      )}
    </Panel>
  )
}


function NewMaintenance({ s, refresh }: { s: Snapshot; refresh: () => void }) {
  const [title, setTitle] = useState("")
  const [starts, setStarts] = useState("")
  const [ends, setEnds] = useState("")
  const [components, setComponents] = useState<string[]>([])
  const [body, setBody] = useState("")
  return (
    <FormDialog title="Maintenance window" trigger="Schedule maintenance" submit="Schedule" valid={!!title.trim() && !!starts && !!ends && components.length > 0 && new Date(ends) > new Date(starts)}
      run={() => send("POST", "/status/maintenance", { title, body, components, starts: new Date(starts).getTime(), ends: new Date(ends).getTime() })} onDone={() => { setTitle(""); setBody(""); setComponents([]); refresh() }}>
      <TextField label="Title" value={title} onChange={setTitle} maxLength={140} />
      <TextField label="Starts" type="datetime-local" value={starts} onChange={setStarts} />
      <TextField label="Ends" type="datetime-local" value={ends} onChange={setEnds} hint="At most 7 days. Downtime in the window does not count against uptime." />
      <Components s={s} value={components} onChange={setComponents} />
      <Notes label="Details" value={body} onChange={setBody} />
    </FormDialog>
  )
}

export function StatusMaintenance({ s, refresh }: { s: Snapshot; refresh: () => void }) {
  const names = new Map(s.public.components.map((c) => [c.id, c.name]))
  const [error, setError] = useState<string | null>(null)
  async function cancel(id: string) {
    try { await send("POST", `/status/maintenance/${id}/cancel`, {}); refresh() } catch (e) { setError(e instanceof ApiError ? e.message : "The request failed.") }
  }
  return (
    <Panel title="Maintenance" action={<NewMaintenance s={s} refresh={refresh} />}>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {s.public.maintenance.length === 0 ? (
        <p className="text-sm text-muted-foreground">No maintenance planned.</p>
      ) : (
        <div className="divide-y divide-divider border-y border-divider text-sm">
          {s.public.maintenance.map((m) => (
            <div key={m.id} className="flex flex-wrap items-center justify-between gap-2 py-3" data-status-maintenance={m.id}>
              <span><span className="font-medium">{m.title}</span> <span className="text-xs text-muted-foreground">{m.components.map((c) => names.get(c) ?? c).join(", ")} · <Time iso={iso(m.starts)} /> to <Time iso={iso(m.ends)} /></span></span>
              <Button size="sm" variant="outline" onClick={() => cancel(m.id)}>Cancel</Button>
            </div>
          ))}
        </div>
      )}
    </Panel>
  )
}
