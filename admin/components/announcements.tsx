"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { BannerPreview, NoticePreview } from "@/components/announcement-preview"
import { SelectField, TextField } from "@/components/fields"
import { SearchBox, StatusBadge } from "@/components/filters"
import type { DialogControl } from "@/components/row-menu"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { operatorAction, useApiQuery, type Data } from "@/lib/api"
import { useAllPages } from "@/lib/paging"
import { useNow } from "@/lib/use-now"

export type Announcement = Data<"/v1/admin/announcements">["announcements"][number]
type Products = Data<"/v1/admin/billing/products">
type Kind = Announcement["kind"]
type Audience = Announcement["audience_kind"]

const audiences: [Audience, string][] = [["all", "Everyone"], ["plan", "A plan"], ["team", "A team"], ["user", "One user"]]
const severities: [string, string][] = [["info", "Info"], ["warning", "Warning"], ["critical", "Critical"]]

/** An <input type=datetime-local> value as RFC 3339 UTC: "" when empty, undefined when it is not a time. */
const isoOf = (local: string): string | undefined => {
  if (!local) return ""
  const parsed = Date.parse(local)
  return Number.isNaN(parsed) ? undefined : new Date(parsed).toISOString()
}
/** An RFC 3339 time as the datetime-local input shows it (the browser's zone). */
const localOf = (iso?: string) => {
  if (!iso) return ""
  const d = new Date(iso)
  return new Date(d.getTime() - d.getTimezoneOffset() * 60_000).toISOString().slice(0, 16)
}

/** Who gets it: everyone, a plan, or one team or user found by search. */
export function AudiencePicker({ kind, value, onKind, onValue, options = audiences }: { kind: string; value: string; onKind: (k: string) => void; onValue: (v: string) => void; options?: [string, string][] }) {
  const [q, setQ] = useState("")
  const products = useAllPages<Products["products"][number]>(kind === "plan" ? "/v1/admin/billing/products" : null, (page) => (page as unknown as Products).products)
  const path = kind === "team" ? "teams" : "accounts"
  const found = useApiQuery<{ teams?: { id: string; owner_account: string }[]; accounts?: { id: string; primary_email: string }[] }>(["announcement-audience", kind, q], (kind === "team" || kind === "user") && q ? `/v1/admin/${path}?q=${encodeURIComponent(q)}&limit=6` : null)
  const rows: [string, string][] = kind === "team" ? (found.data?.teams ?? []).map((t) => [t.id, t.id]) : (found.data?.accounts ?? []).map((a) => [a.id, a.primary_email])
  const plans: [string, string][] = [["free", "Free (no paid personal plan)"], ...(products.data?.items ?? []).filter((p) => p.active).map((p): [string, string] => [p.code, `${p.display_name} (${p.code})`])]
  return (
    <div className="flex flex-col gap-3" data-audience-picker>
      <SelectField label="Who gets it" value={kind} onChange={(k) => { onKind(k); onValue(""); setQ("") }} options={options} />
      {kind === "plan" && <SelectField label="Plan" value={value} onChange={onValue} options={[["", products.isPending ? "Loading…" : "Choose a plan"], ...plans]} />}
      {(kind === "team" || kind === "user") && (
        <div className="flex flex-col gap-1.5">
          <Label>{kind === "team" ? "Team" : "User"}</Label>
          {value ? (
            <div className="flex items-center justify-between gap-2 rounded-md bg-surface px-2.5 py-1.5 text-sm" data-audience-chosen>
              <span className="min-w-0 truncate font-mono text-xs">{value}</span>
              <Button type="button" variant="ghost" size="sm" onClick={() => onValue("")}>Change</Button>
            </div>
          ) : (
            <>
              <SearchBox label={`Search ${kind}s`} placeholder={kind === "team" ? "Team id" : "Email, name or id"} value={q} onChange={setQ} />
              {q && (
                <ul className="flex flex-col" aria-label="Matches">
                  {rows.map(([id, label]) => (
                    <li key={id}>
                      <button type="button" className="w-full truncate rounded-sm px-2 py-1.5 text-left text-sm outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring/30" onClick={() => onValue(id)}>{label}</button>
                    </li>
                  ))}
                  {found.isSuccess && rows.length === 0 && <li className="px-2 py-1.5 text-sm text-muted-foreground">No match.</li>}
                </ul>
              )}
            </>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * Write or edit an announcement: a notice (the console bell) or a ribbon (above every page), its audience and
 * time, with the preview as the console shows it. Editing keeps the kind.
 */
export function AnnouncementDialog({ existing, control }: { existing?: Announcement; control?: DialogControl }) {
  const [kind, setKind] = useState<Kind>(existing?.kind ?? "notice")
  const [title, setTitle] = useState(existing?.title ?? "")
  const [body, setBody] = useState(existing?.body ?? "")
  const [link, setLink] = useState(existing?.link_url ?? "")
  const [severity, setSeverity] = useState(existing?.severity ?? "info")
  const [dismissible, setDismissible] = useState(existing?.dismissible ?? true)
  const [audience, setAudience] = useState<Audience>(existing?.audience_kind ?? "all")
  const [value, setValue] = useState(existing?.audience_value ?? "")
  const [starts, setStarts] = useState(localOf(existing?.starts_at))
  const [ends, setEnds] = useState(localOf(existing?.ends_at))
  const now = useNow()
  const startsAt = isoOf(starts)
  const endsAt = isoOf(ends)
  const timesOk = startsAt !== undefined && endsAt !== undefined && (!endsAt || endsAt > (startsAt || new Date(now).toISOString()))
  const linkOk = link === "" || /^(https:\/\/\S+|\/(?!\/)\S*)$/.test(link.trim())
  const valid = title.trim() !== "" && title.trim().length <= 80 && body.trim().length <= 280 && linkOk && timesOk && (audience === "all" || value !== "")
  const input = { kind, title: title.trim(), body: body.trim(), link_url: link.trim(), severity, dismissible, audience_kind: audience, audience_value: audience === "all" ? "" : value, ...(startsAt ? { starts_at: startsAt } : {}), ...(endsAt ? { ends_at: endsAt } : {}) }
  const edit = existing !== undefined
  // A new announcement starts empty each time; an edit is mounted only while open, so it starts from the row.
  const [open, setOpen] = useState(false)
  const reset = () => {
    setKind("notice"); setTitle(""); setBody(""); setLink(""); setSeverity("info"); setDismissible(true)
    setAudience("all"); setValue(""); setStarts(""); setEnds("")
  }
  return (
    <ActionDialog
      {...(edit ? control : { open, onOpenChange: (next: boolean) => { setOpen(next); if (!next) reset() } })}
      title={edit ? "Edit announcement" : "New announcement"}
      description={edit ? "Changes show to users at once. Read and dismissed marks stay." : "Shown in the console to the people you pick, between the start and the end."}
      target={edit ? existing.id : "announcements"}
      submitLabel={edit ? "Save" : "Send"}
      valid={valid}
      trigger={edit ? undefined : (open) => <Button size="lg" onClick={open} data-action="new-announcement">New announcement</Button>}
      run={(h) => (edit ? operatorAction("PUT", `/v1/admin/announcements/${encodeURIComponent(existing.id)}`, h, input) : operatorAction("POST", "/v1/admin/announcements", h, input))}
    >
      <SelectField label="Where" value={kind} disabled={edit} onChange={(k) => setKind(k as Kind)} options={[["notice", "Notice in the bell"], ["banner", "Banner above every page"]]} />
      <TextField label="Title" value={title} onChange={setTitle} maxLength={80} hint={`${title.trim().length}/80`} />
      <div className="flex flex-col gap-1.5">
        <div className="flex items-baseline justify-between gap-2">
          <Label htmlFor="announcement-body">Text</Label>
          <span className="text-xs text-muted-foreground">Optional</span>
        </div>
        <Textarea id="announcement-body" value={body} onChange={(e) => setBody(e.target.value)} maxLength={280} rows={3} />
        <span className="text-xs text-muted-foreground">{body.trim().length}/280</span>
      </div>
      <TextField label="Link" optional value={link} onChange={setLink} placeholder="https://… or /billing" hint={linkOk ? "An https address or a console page." : "Use an https address or a path such as /billing."} />
      {kind === "banner" && (
        <div className="flex flex-wrap items-end gap-4">
          <div className="min-w-32 flex-1"><SelectField label="Severity" value={severity} onChange={(s) => setSeverity(s as typeof severity)} options={severities} /></div>
          <label className="flex items-center gap-2 pb-1 text-sm"><Checkbox checked={dismissible} onCheckedChange={(v) => setDismissible(v === true)} />Users can dismiss it</label>
        </div>
      )}
      <AudiencePicker kind={audience} value={value} onKind={(k) => setAudience(k as Audience)} onValue={setValue} />
      <TextField label="Starts" optional type="datetime-local" value={starts} onChange={setStarts} hint="Empty starts now." />
      <TextField label="Ends" optional type="datetime-local" value={ends} onChange={setEnds} hint={timesOk ? "Empty keeps it until you retract it." : "It must end after it starts."} />
      <div className="flex flex-col gap-1.5">
        <Label>Preview</Label>
        {kind === "banner" ? <BannerPreview title={title.trim()} body={body.trim()} link={link.trim()} severity={severity} dismissible={dismissible} /> : <NoticePreview title={title.trim()} body={body.trim()} link={link.trim()} />}
      </div>
    </ActionDialog>
  )
}

/** End an announcement now for everyone. */
export function RetractDialog({ item, control }: { item: Announcement; control: DialogControl }) {
  return (
    <ActionDialog
      {...control}
      title={`Retract “${item.title}”`}
      description="It disappears for everyone at once. It stays in this list, and the audit log keeps who retracted it."
      target={item.id}
      destructive
      submitLabel="Retract"
      run={(h) => operatorAction("POST", `/v1/admin/announcements/${encodeURIComponent(item.id)}/retract`, h, {})}
    />
  )
}

export const audienceLabel = (a: Announcement) => (a.audience_kind === "all" ? "Everyone" : `${a.audience_kind === "plan" ? "Plan" : a.audience_kind === "team" ? "Team" : "User"} ${a.audience_value}`)
export const AnnouncementState = ({ item }: { item: Announcement }) => <StatusBadge state={item.state} />
