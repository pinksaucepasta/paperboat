"use client"

import { BellIcon } from "@phosphor-icons/react"
import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { ListPager } from "@/components/list-pager"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { AnnouncementLink } from "@/components/announcement-banner"
import { useApiQuery, useWorkspace, workspacesQuery } from "@/lib/api"
import { announcementsQuery, markNoticesRead, type AnnouncementNotice } from "@/lib/announcements"
import { decideRequest, type InboxRequest } from "@/lib/files"
import { formatBytes } from "@/lib/format"
import {
  answersMachines,
  machineRequestsOf,
  requestKinds,
  requestsPageSize,
  teamRequestsQuery,
  waitingFilesQuery,
  waitingInvitationsQuery,
  type MachineRequest,
  type RequestKind,
} from "@/lib/requests"
import { answerInvitation, answerMachine, roleLabels, type TeamInvitation } from "@/lib/teams"
import { queryKeys } from "@/lib/query-keys"
import { useTrack } from "@/lib/analytics"
import { ErrorMessage } from "@/components/error-reference"

/**
 * The header's bell: things waiting for you to accept or decline (files teammates want to send, team
 * invitations, machines members asked to add) and Paperboat's announcements. The bell is always there,
 * like any inbox; the badge shows only while something waits or is unread.
 */
export function RequestsMenu() {
  const workspace = useWorkspace()?.id ?? ""
  const files = useApiQuery({ ...waitingFilesQuery(workspace, 1), enabled: Boolean(workspace) })
  const invitations = useApiQuery(waitingInvitationsQuery(1))
  const teams = useApiQuery(workspacesQuery).data?.filter(answersMachines) ?? []
  const summaries = useQueries({ queries: teams.map((team) => teamRequestsQuery(team.id)) })
  const machines = machineRequestsOf(summaries.map((summary) => summary.data))
  const announcements = useApiQuery(announcementsQuery).data
  const counts: Record<RequestKind, number> = {
    files: files.data?.total ?? 0,
    invitations: invitations.data?.total ?? 0,
    machines: machines.length,
    announcements: announcements?.unread ?? 0,
  }
  const total = counts.files + counts.invitations + counts.machines + counts.announcements
  return (
    <Popover>
      <PopoverTrigger
        render={<Button variant="ghost" size="icon" className="relative" aria-label={total ? `Requests, ${total} waiting` : "Requests"} />}
      >
        <BellIcon />
        {total > 0 && <span className="absolute -top-0.5 -right-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-primary px-1 text-[0.625rem] leading-none font-medium text-primary-foreground tabular-nums">
          {total > 99 ? "99+" : total}
        </span>}
      </PopoverTrigger>
      <PopoverContent align="end" className="max-h-[min(36rem,calc(100dvh-5rem))] w-[min(26rem,calc(100vw-2rem))] gap-2 overflow-y-auto p-3 text-sm">
        <RequestList counts={counts} machines={machines} notices={announcements?.notices ?? []} workspace={workspace} />
      </PopoverContent>
    </Popover>
  )
}

function RequestList({
  counts,
  machines,
  notices,
  workspace,
}: {
  counts: Record<RequestKind, number>
  machines: MachineRequest[]
  notices: AnnouncementNotice[]
  workspace: string
}) {
  // Read announcements stay listed: only the unread count is a number to answer.
  const kinds = requestKinds.filter((kind) => counts[kind.id] > 0 || (kind.id === "announcements" && notices.length > 0))
  const [chosen, setChosen] = useState<RequestKind>()
  const [asked, setPage] = useState(1)
  const kind = kinds.find((entry) => entry.id === chosen)?.id ?? kinds[0]?.id
  if (!kind) {
    return (
      <div className="flex flex-col items-center gap-1 px-4 py-6 text-center">
        <BellIcon className="size-6 text-muted-foreground" />
        <p className="font-medium">Nothing waiting</p>
        <p className="text-muted-foreground">
          Files teammates send you, team invitations, machine requests and news from Paperboat show up here.
        </p>
      </div>
    )
  }
  // Answering the last row of the last page leaves that page empty: show the one before it.
  const page = Math.min(asked, Math.max(1, Math.ceil(counts[kind] / requestsPageSize)))
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap gap-1" role="tablist" aria-label="Requests">
        {kinds.map((entry) => (
          <Button
            key={entry.id}
            role="tab"
            aria-selected={entry.id === kind}
            variant={entry.id === kind ? "secondary" : "ghost"}
            onClick={() => {
              setChosen(entry.id)
              setPage(1)
            }}
          >
            {entry.label}{" "}
            {counts[entry.id] > 0 && <span className="tabular-nums text-muted-foreground">{counts[entry.id]}</span>}
          </Button>
        ))}
      </div>
      {kind === "files" && <FileRequests workspace={workspace} total={counts.files} page={page} go={setPage} />}
      {kind === "invitations" && <InvitationRequests total={counts.invitations} page={page} go={setPage} />}
      {kind === "machines" && <MachineRequests rows={machines} page={page} go={setPage} />}
      {kind === "announcements" && <Notices notices={notices} unread={counts.announcements} />}
    </div>
  )
}

const rowClass = "flex flex-col gap-2 border-b border-divider py-3 last:border-b-0"

function Answer({
  pending,
  error,
  answer,
  accept = "Accept",
}: {
  pending: boolean
  error?: Error | null
  answer: (reply: "accept" | "decline") => void
  accept?: string
}) {
  return (
    <>
      {error && (
        <span className="text-xs text-destructive">
          <ErrorMessage error={error} />
        </span>
      )}
      <div className="flex gap-2">
        <Button variant="outline" disabled={pending} onClick={() => answer("decline")}>
          Decline
        </Button>
        <Button disabled={pending} onClick={() => answer("accept")}>
          {accept}
        </Button>
      </div>
    </>
  )
}

function FileRequests({ workspace, total, page, go }: { workspace: string; total: number; page: number; go: (page: number) => void }) {
  const requests = useApiQuery({ ...waitingFilesQuery(workspace, page), placeholderData: (previous) => previous })
  return (
    <>
      {requests.data?.rows.map((request) => <FileRequest key={request.request_id} request={request} workspace={workspace} />)}
      <ListPager page={page} size={requestsPageSize} total={total} go={go} />
    </>
  )
}

function FileRequest({ request, workspace }: { request: InboxRequest; workspace: string }) {
  const queryClient = useQueryClient()
  const decide = useMutation({
    mutationFn: (reply: "accept" | "decline") => decideRequest(request, reply === "accept" ? "approve" : "decline"),
    onSuccess: async (_, reply) => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.inboxRequests(workspace) })
      toast.success(reply === "accept" ? "Accepted" : "Declined")
    },
  })
  const [first, ...rest] = request.files
  const size = formatBytes(request.files.reduce((sum, file) => sum + file.size, 0))
  return (
    <div className={rowClass}>
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="truncate">
          <span className="font-medium">{first?.basename}</span>
          {rest.length > 0 && ` and ${rest.length} more`}
        </span>
        <span className="text-xs text-muted-foreground">
          {size} from {request.source_machine_name || "a teammate’s machine"} to{" "}
          {request.destination_machine_name || "your machine"} · expires <Time iso={request.expires_at} relative />
        </span>
      </div>
      <Answer pending={decide.isPending} error={decide.error} answer={decide.mutate} />
    </div>
  )
}

function InvitationRequests({ total, page, go }: { total: number; page: number; go: (page: number) => void }) {
  const invitations = useApiQuery({ ...waitingInvitationsQuery(page), placeholderData: (previous) => previous })
  return (
    <>
      {invitations.data?.rows.map((invitation) => <InvitationRequest key={invitation.invitation_id} invitation={invitation} />)}
      <ListPager page={page} size={requestsPageSize} total={total} go={go} />
    </>
  )
}

function InvitationRequest({ invitation }: { invitation: TeamInvitation }) {
  const queryClient = useQueryClient()
  const track = useTrack()
  const answer = useMutation({
    mutationFn: (reply: "accept" | "decline") => answerInvitation(invitation.invitation_id, reply),
    onSuccess: async (_, reply) => {
      if (reply === "accept") track("invitation_accepted", { role: invitation.role })
      await queryClient.invalidateQueries({ queryKey: ["my-invitations"] })
      if (reply === "accept") await queryClient.invalidateQueries({ queryKey: workspacesQuery.queryKey })
      toast.success(reply === "accept" ? `Joined ${invitation.team_id}` : "Declined")
    },
  })
  return (
    <div className={rowClass}>
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="truncate">
          Join <span className="font-medium">{invitation.team_id}</span> as{" "}
          {roleLabels[invitation.role]?.toLowerCase() ?? invitation.role}
        </span>
        <span className="text-xs text-muted-foreground">
          Invited by {invitation.created_by_email} · expires <Time iso={invitation.expires_at} relative />
        </span>
      </div>
      <Answer pending={answer.isPending} error={answer.error} answer={answer.mutate} accept="Join team" />
    </div>
  )
}

function MachineRequests({ rows, page, go }: { rows: MachineRequest[]; page: number; go: (page: number) => void }) {
  const from = (page - 1) * requestsPageSize
  return (
    <>
      {rows.slice(from, from + requestsPageSize).map((row) => (
        <MachineRequestRow key={`${row.team.team_id}/${row.request.machine_id}`} row={row} />
      ))}
      <ListPager page={page} size={requestsPageSize} total={rows.length} go={go} />
    </>
  )
}

function MachineRequestRow({ row }: { row: MachineRequest }) {
  const queryClient = useQueryClient()
  const answer = useMutation({
    mutationFn: (reply: "accept" | "decline") =>
      answerMachine(row.team, row.request.machine_id, reply === "accept" ? "approve" : "decline"),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["team-summary"] })
      await queryClient.invalidateQueries({ queryKey: ["team", row.team.team_id] })
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
    },
  })
  return (
    <div className={rowClass}>
      <span className="truncate">
        <span className="font-medium">{row.request.alias}</span> wants to join{" "}
        <span className="font-medium">{row.team.display_name || row.team.team_id}</span>
      </span>
      <Answer pending={answer.isPending} error={answer.error} answer={answer.mutate} />
    </div>
  )
}

function Notices({ notices, unread }: { notices: AnnouncementNotice[]; unread: number }) {
  const queryClient = useQueryClient()
  const read = useMutation({
    mutationFn: markNoticesRead,
    // Read at once; the server's answer is the same set.
    onMutate: (ids: string[]) => {
      queryClient.setQueryData(announcementsQuery.queryKey, (old) =>
        old
          ? {
              ...old,
              notices: old.notices.map((n) => (ids.includes(n.id) ? { ...n, read: true } : n)),
              unread: old.notices.filter((n) => !n.read && !ids.includes(n.id)).length,
            }
          : old,
      )
    },
    onError: () => queryClient.invalidateQueries({ queryKey: announcementsQuery.queryKey }),
  })
  const unreadIds = notices.filter((n) => !n.read).map((n) => n.id)
  return (
    <div data-announcements>
      {unread > 0 && (
        <Button variant="ghost" className="self-start" disabled={read.isPending} onClick={() => read.mutate(unreadIds)}>
          Mark all as read
        </Button>
      )}
      {notices.map((notice) => (
        <div key={notice.id} className={rowClass} data-notice-read={notice.read}>
          <div className="flex min-w-0 items-baseline gap-2">
            {!notice.read && (
              <span className="size-2 shrink-0 self-center rounded-full bg-primary">
                <span className="sr-only">Unread</span>
              </span>
            )}
            <span className={notice.read ? "min-w-0 break-words" : "min-w-0 break-words font-medium"}>{notice.title}</span>
          </div>
          {notice.body && <p className="break-words text-muted-foreground">{notice.body}</p>}
          <div className="flex items-center gap-3 text-xs text-muted-foreground">
            <Time iso={notice.sent_at} relative />
            {notice.link_url && (
              <span onClickCapture={() => !notice.read && read.mutate([notice.id])}>
                <AnnouncementLink url={notice.link_url}>Learn more</AnnouncementLink>
              </span>
            )}
            {!notice.read && (
              <Button variant="ghost" size="sm" className="ml-auto" disabled={read.isPending} onClick={() => read.mutate([notice.id])}>
                Mark as read
              </Button>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}
