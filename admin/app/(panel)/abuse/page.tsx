"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { Filters, SearchBox } from "@/components/filters"
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
            ["Report id", <span key="i" className="font-mono text-xs">{report.id}</span>],
          ]}
        />
      </DialogContent>
    </Dialog>
  )
}

/** A report's row: read it in full, or take its hostname down (a publication block whose reference is the report id). */
function ReportActions({ report }: { report: Report }) {
  return (
    <RowMenu
      label={report.id}
      data-abuse-actions={report.id}
      actions={[
        { key: "view", label: "View", dialog: (c) => <ReportDetail report={report} {...c} /> },
        {
          key: "block",
          label: "Block hostname",
          destructive: true,
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Block ${report.hostname}`}
              description={`The edge refuses it until it is unblocked. Use ${report.id} as the reference. If it was blocked before, unblock or block it again from Servers → Hostnames.`}
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
  const filters = useMemo(() => ({ hostname, reason }), [hostname, reason])
  const list = useCursorList<Reports>("/v1/admin/abuse-reports", filters, "cursor")
  return (
    <Page title="Abuse reports">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Hostname" placeholder="Exact hostname" value={hostname} onChange={setHostname} />
        <Filters items={[{ label: "Reason", value: reason, onChange: setReason, options: reasons }]} />
      </div>
      <DataTable
        what="abuse reports"
        list={list}
        rows={(d) => d.reports}
        rowKey={(r) => r.id}
        empty={hostname || reason ? "No reports match." : "No abuse reports."}
        columns={[
          { header: "Hostname", cell: (r) => <span className="font-medium">{r.hostname}</span> },
          { header: "Reason", cell: (r) => reasonText(r.reason) },
          { header: "Link", className: "hidden md:table-cell", cell: (r) => <span className="font-mono text-xs text-muted-foreground">{r.url}</span> },
          { header: "Reported", cell: (r) => <Time iso={r.created_at} relative /> },
          { header: "", className: "w-12", cell: (r) => <ReportActions report={r} /> },
        ]}
      />
    </Page>
  )
}
