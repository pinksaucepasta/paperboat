"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { SelectField, TextField } from "@/components/fields"
import { Mono, StatusBadge } from "@/components/filters"
import { Facts, Panel } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { operatorAction, useApiQuery, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

/** A release version as the server accepts it: YYYY.M.D.build with a real calendar date. */
export function dateVersion(value: string): boolean {
  const m = /^(\d{4})\.(\d{1,2})\.(\d{1,2})\.(\d{1,6})$/.exec(value)
  if (!m || +m[1] < 1) return false
  const [year, month, day] = [+m[1], +m[2], +m[3]]
  return month >= 1 && month <= 12 && day >= 1 && day <= new Date(Date.UTC(year, month, 0)).getUTCDate()
}
const platforms: [string, string][] = [["darwin/arm64", "macOS (Apple silicon)"], ["linux/amd64", "Linux x86-64"], ["linux/arm64", "Linux ARM64"], ["windows/amd64", "Windows x86-64"], ["windows/arm64", "Windows ARM64"]]
const actions: [string, string][] = [["promote", "Promote (roll out)"], ["pause", "Pause"], ["quarantine", "Quarantine"], ["revoke", "Revoke"]]

/** The oldest CLI version the server still serves; older CLIs are told to update. A fleet-wide gate. */
export function ClientFloor() {
  const q = useApiQuery<Data<"/v1/admin/client-update-policy">>(["client-update-policy"], "/v1/admin/client-update-policy")
  const [version, setVersion] = useState("")
  const [reason, setReason] = useState("")
  const [enforceAt, setEnforceAt] = useState("")
  if (q.isError) return <ErrorBlock what="the client update policy" error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-24 w-full" />
  const policy = q.data
  const clearing = version.trim() === ""
  const valid = (clearing || dateVersion(version.trim())) && (clearing || (reason.trim() !== "" && new TextEncoder().encode(reason).length <= 256)) && (enforceAt === "" || !Number.isNaN(Date.parse(enforceAt)))
  return (
    <div data-client-floor>
      <Panel
        title="Minimum CLI version"
        action={
        <ActionDialog
          title="Change the minimum CLI version"
          description="Older CLIs are told to update on their next request. The version must be published for every platform."
          target="client-update-policy"
          destructive
          confirmWord="update-policy"
          submitLabel={clearing ? "Remove minimum" : "Set minimum"}
          valid={valid}
          trigger={(open) => <Button variant="outline" size="lg" onClick={() => { setVersion(policy.minimum_version); setReason(""); setEnforceAt(""); open() }}>Change minimum version</Button>}
          run={(h) => operatorAction("PUT", "/v1/admin/client-update-policy", h, { expected_revision: policy.revision, minimum_version: version.trim(), reason: clearing ? "" : reason.trim(), enforce_at: enforceAt ? new Date(enforceAt).toISOString() : null })}
        >
          <TextField label="Minimum version" value={version} onChange={setVersion} placeholder="2026.11.18.1" hint="YYYY.M.D.build. Empty for none." />
          {!clearing && <TextField label="Reason shown to users" value={reason} onChange={setReason} maxLength={256} hint="Public. Up to 256 bytes." />}
          {!clearing && <TextField label="Enforce from" optional type="datetime-local" value={enforceAt} onChange={setEnforceAt} hint="Empty for now." />}
        </ActionDialog>
        }
      >
        <Facts
          rows={[
            ["Version", policy.minimum_version || "None"],
            ["Reason", policy.reason || "—"],
            ["Enforced from", policy.enforce_at ? <Time key="e" iso={policy.enforce_at} /> : "—"],
            ["Revision", policy.revision],
          ]}
        />
      </Panel>
    </div>
  )
}

function RequestRelease() {
  const [action, setAction] = useState("promote")
  const [release, setRelease] = useState("")
  const [version, setVersion] = useState("")
  const [target, setTarget] = useState("linux/amd64")
  const [revision, setRevision] = useState("")
  const [percent, setPercent] = useState("100")
  const promote = action === "promote"
  const pct = Number(percent)
  const valid = release.trim() !== "" && release.length <= 128 && dateVersion(version) && /^[1-9]\d{0,14}$/.test(revision) && (!promote || (Number.isInteger(pct) && pct >= 1 && pct <= 100))
  const [platform, architecture] = target.split("/")
  return (
    <ActionDialog
      title="Request a release decision"
      description="The release authority must sign it. Nothing changes until the signed bundle is imported."
      target={release || "-"}
      submitLabel="Create request"
      valid={valid}
      trigger={(open) => <Button variant="outline" size="lg" onClick={open}>Request a release decision</Button>}
      run={(h) => operatorAction("POST", "/v1/admin/release-authority/requests", h, { action, release_id: release.trim(), version, platform, architecture, policy_revision: Number(revision), rollout_percentage: promote ? pct : 0 })}
    >
      <SelectField label="Action" value={action} onChange={setAction} options={actions} />
      <TextField label="Release id" value={release} onChange={setRelease} maxLength={128} />
      <TextField label="Version" value={version} onChange={setVersion} placeholder="2026.11.18.1" hint="YYYY.M.D.build" />
      <SelectField label="Platform" value={target} onChange={setTarget} options={platforms} />
      <TextField label="Policy revision" value={revision} onChange={setRevision} inputMode="numeric" hint="Higher than the last imported one." />
      {promote && <TextField label="Rollout percentage" value={percent} onChange={setPercent} inputMode="numeric" hint="1–100" />}
    </ActionDialog>
  )
}

// The OpenAPI document types these two lists as plain objects, so the shapes (releaseauthority.Request/Record) are written here.
type Decision = { release_id: string; action: string; version: string; platform: string; architecture: string; policy_revision: number; rollout_percentage: number }
type Pagination = { pagination: { total: number; next_offset: number | null } }
type Requests = Pagination & { requests: (Decision & { id: string; status: string; created_at: string })[] }
type Bundles = Pagination & { bundles: (Decision & { id: string; imported_at: string; expires_at: string })[] }

const bundleMax = 64 << 10

function ImportBundle() {
  const [text, setText] = useState("")
  const parsed = useMemo(() => {
    if (!text.trim() || new TextEncoder().encode(text).length > bundleMax) return undefined
    try {
      const value: unknown = JSON.parse(text)
      return value && typeof value === "object" && !Array.isArray(value) ? (value as { release_id?: unknown }) : undefined
    } catch {
      return undefined
    }
  }, [text])
  const release = typeof parsed?.release_id === "string" ? parsed.release_id : ""
  return (
    <ActionDialog
      title="Import a signed release bundle"
      description="Updaters act on it at once. It can’t be taken back; only a newer signed decision replaces it."
      target={release || "-"}
      destructive
      confirmWord={release || "release"}
      submitLabel="Import bundle"
      valid={release !== ""}
      trigger={(open) => <Button variant="outline" size="lg" onClick={open}>Import a signed bundle</Button>}
      run={(h) => operatorAction("POST", "/v1/admin/release-authority/bundles", h, parsed)}
      onDone={() => setText("")}
    >
      <div className="flex flex-col gap-1.5">
        <label htmlFor="bundle-json" className="text-sm font-medium">Signed bundle</label>
        <Textarea id="bundle-json" rows={8} className="font-mono text-xs" value={text} onChange={(e) => setText(e.target.value)} />
        <span className="text-xs text-muted-foreground">{text.trim() === "" ? "JSON, up to 64 KiB." : release ? `Release ${release}` : "Not a bundle: needs a JSON object with release_id, up to 64 KiB."}</span>
      </div>
    </ActionDialog>
  )
}

/** Requests the panel created and the signed decisions imported, with the two actions that write them. */
export function ReleaseAuthority() {
  const filters = useMemo(() => ({}), [])
  const requests = useCursorList<Requests>("/v1/admin/release-authority/requests", filters, "offset")
  const bundles = useCursorList<Bundles>("/v1/admin/release-authority/bundles", filters, "offset")
  return (
    <div className="flex flex-col gap-8" data-release-authority>
      <Panel title="Requests" action={<RequestRelease />}>
        <DataTable
          what="release requests"
          list={requests}
          rows={(d) => d.requests}
          rowKey={(r) => r.id}
          empty="No requests yet."
          columns={[
            { header: "Release", cell: (r) => <Mono>{r.release_id}</Mono> },
            { header: "Action", cell: (r) => (r.action === "promote" ? `promote ${r.rollout_percentage}%` : r.action) },
            { header: "Version", cell: (r) => `${r.version} · ${r.platform}/${r.architecture}` },
            { header: "Revision", cell: (r) => r.policy_revision, className: "tabular-nums" },
            { header: "Status", cell: (r) => <StatusBadge state={r.status} /> },
            { header: "Created", cell: (r) => <Time iso={r.created_at} relative /> },
          ]}
        />
      </Panel>
      <Panel title="Imported decisions" action={<ImportBundle />}>
        <DataTable
          what="release decisions"
          list={bundles}
          rows={(d) => d.bundles}
          rowKey={(b) => b.id}
          empty="No decisions imported yet."
          columns={[
            { header: "Release", cell: (b) => <Mono>{b.release_id}</Mono> },
            { header: "Action", cell: (b) => (b.action === "promote" ? `promote ${b.rollout_percentage}%` : b.action) },
            { header: "Version", cell: (b) => `${b.version} · ${b.platform}/${b.architecture}` },
            { header: "Revision", cell: (b) => b.policy_revision, className: "tabular-nums" },
            { header: "Imported", cell: (b) => <Time iso={b.imported_at} relative /> },
            { header: "Expires", cell: (b) => <Time iso={b.expires_at} /> },
          ]}
        />
      </Panel>
    </div>
  )
}
