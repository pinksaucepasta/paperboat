"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { Filters, SearchBox, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Facts, Page } from "@/components/page"
import { RowMenu, type DialogControl } from "@/components/row-menu"
import { Time } from "@/components/time"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { operatorAction, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Reports = Data<"/v1/admin/abuse-reports">
type Report = Reports["reports"][number]

const reasons: [string, string][] = [["phishing", "Phishing"], ["malware", "Malware"], ["spam", "Spam"], ["illegal", "Illegal"], ["other", "Other"]]
const reasonText = (r: string) => reasons.find(([v]) => v === r)?.[1] ?? r
const statuses: [string, string][] = [["open", "Open"], ["actioned", "Actioned"], ["dismissed", "Dismissed"]]

function ReportDetail({ report, ...control }: { report: Report } & DialogControl) {
  return (
    <Dialog {...control}>
      <DialogContent className="gap-4 sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{report.hostname}</DialogTitle>
        </DialogHeader>
        <Facts
          rows={[
            ["Reason", reasonText(report.reason)],
            ["Link", <span key="u" className="break-all font-mono text-xs">{report.url}</span>],
            ["Details", report.details || "—"],
            ["Reporter", report.reporter_email || "Anonymous"],
            ["Reported", <Time key="t" iso={report.created_at} />],
            ["Status", <StatusBadge key="s" state={report.status} />],
            ...(report.triaged_at ? [["Triaged", <span key="g">{report.triaged_by} · <Time iso={report.triaged_at} /></span>] as [string, React.ReactNode]] : []),
            ...(report.triage_note ? [["Note", report.triage_note] as [string, React.ReactNode]] : []),
            ["Report id", <span key="i" className="font-mono text-xs">{report.id}</span>],
          ]}
        />
      </DialogContent>
    </Dialog>
  )
}

/** Set a report's status with an optional note (audited with the reference). */
function TriageDialog({ report, status, title, description, ...control }: { report: Report; status: string; title: string; description: string } & DialogControl) {
  const [note, setNote] = useState("")
  return (
    <ActionDialog
      {...control}
      title={title}
      description={description}
      target={report.id}
      submitLabel={title}
      valid={note.length <= 1000}
      run={(h) => operatorAction("POST", `/v1/admin/abuse-reports/${encodeURIComponent(report.id)}/triage`, h, { status, note })}
    >
      <TextField label="Note" optional value={note} onChange={setNote} maxLength={1000} />
    </ActionDialog>
  )
}

/** A report's row: read it in full, triage it, or take its hostname down (a block whose reference is the report id
 * marks the report actioned). */
function ReportActions({ report }: { report: Report }) {
  const triage = (status: string, label: string, description: string) => ({
    key: status,
    label,
    dialog: (c: DialogControl) => <TriageDialog report={report} status={status} title={label} description={description} {...c} />,
  })
  return (
    <RowMenu
      label={report.id}
      data-abuse-actions={report.id}
      actions={[
        { key: "view", label: "View", dialog: (c) => <ReportDetail report={report} {...c} /> },
        ...(report.status === "open"
          ? [triage("dismissed", "Dismiss report", "The report leaves the open queue. Nothing changes for the hostname."), triage("actioned", "Mark report actioned", "For a report handled outside this panel.")]
          : [triage("open", "Reopen report", "The report goes back to the open queue; its note is cleared.")]),
        {
          key: "block",
          label: "Block hostname",
          destructive: true,
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Block ${report.hostname}`}
              description={`The edge refuses it until it is unblocked, and this report is marked actioned. Use ${report.id} as the reference. If it was blocked before, block it again from Servers → Hostnames.`}
              target={report.hostname}
              destructive
              confirmWord={report.hostname}
              submitLabel="Block hostname"
              run={(h) => operatorAction("POST", `/v1/admin/publications/${encodeURIComponent(report.hostname)}/block`, h, { revision: 1 })}
            />
          ),
        },
      ]}
    />
  )
}

/** Abuse reports anyone filed about a link Paperboat serves, newest first (kept 180 days). */
export default function AbusePage() {
  const [hostname, setHostname] = useState("")
  const [reason, setReason] = useState("")
  // The queue: open reports by default; clearing the chip shows every status.
  const [status, setStatus] = useState("open")
  const filters = useMemo(() => ({ hostname, reason, status }), [hostname, reason, status])
  const list = useCursorList<Reports>("/v1/admin/abuse-reports", filters, "cursor")
  return (
    <Page title="Abuse reports">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Hostname" placeholder="Exact hostname" value={hostname} onChange={setHostname} />
        <Filters items={[{ label: "Status", value: status, onChange: setStatus, options: statuses }, { label: "Reason", value: reason, onChange: setReason, options: reasons }]} />
      </div>
      <DataTable
        what="abuse reports"
        list={list}
        rows={(d) => d.reports}
        rowKey={(r) => r.id}
        empty={hostname || reason ? "No reports match." : status === "open" ? "No open reports." : status ? "No reports match." : "No abuse reports."}
        columns={[
          { header: "Hostname", cell: (r) => <span className="font-medium">{r.hostname}</span> },
          { header: "Reason", cell: (r) => reasonText(r.reason) },
          { header: "Link", className: "hidden md:table-cell", cell: (r) => <span className="font-mono text-xs text-muted-foreground">{r.url}</span> },
          { header: "Status", cell: (r) => <StatusBadge state={r.status} /> },
          { header: "Reported", className: "hidden md:table-cell", cell: (r) => <Time iso={r.created_at} relative /> },
          { header: "", className: "w-12", cell: (r) => <ReportActions report={r} /> },
        ]}
      />
    </Page>
  )
}
