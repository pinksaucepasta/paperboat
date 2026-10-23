"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { ApiError, useApiQuery } from "@/lib/api"
import {
  auditStreamsQuery,
  removeAuditStream,
  saveAuditStream,
  testAuditStream,
  type AuditStream,
  type AuditStreamInput,
  type StreamKind,
} from "@/lib/audit-streams"
import { useTimeFormat } from "@/lib/use-time-format"
import { CopyRow } from "@/components/copy-button"
import { ConfirmAction } from "@/components/list-toolbar"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

/*
 * Activity → Streaming: the team's audit log sent to a SIEM as it happens, at most one HTTPS and
 * one S3-compatible destination (server/docs/teams-business.md "Streaming to a SIEM"). Owners and
 * admins set them up; auditors see their status.
 */

const kinds: Record<StreamKind, { title: string; about: string }> = {
  https: {
    title: "HTTPS",
    about: "Batches of events as NDJSON, POSTed to your SIEM (Splunk HEC, Datadog, Axiom, Elastic…), signed with a secret.",
  },
  s3: {
    title: "S3-compatible storage",
    about: "One gzip NDJSON file per batch in your bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO).",
  },
}

export function AuditStreams({ team, canConfigure }: { team: string; canConfigure: boolean }) {
  const streams = useApiQuery(auditStreamsQuery(team))
  if (streams.isPending) return <Skeleton className="h-40 w-full" />
  if (streams.isError) {
    const plan = streams.error instanceof ApiError && streams.error.code === "team_plan_required"
    return (
      <p className="py-6 text-sm text-muted-foreground">
        {plan ? "Streaming the audit log is part of the Team plan." : streams.error.message}
      </p>
    )
  }
  return (
    <div className="flex flex-col gap-10">
      <p className="max-w-2xl text-sm text-muted-foreground">
        Send this team’s audit log to your security tools as it happens. A new stream starts with events after it is set
        up; each arrives within 30 seconds, at least once and in order.
      </p>
      {(["https", "s3"] as const).map((kind) => (
        <Destination
          key={kind}
          team={team}
          kind={kind}
          stream={streams.data.find((stream) => stream.kind === kind)}
          canConfigure={canConfigure}
        />
      ))}
    </div>
  )
}

function Destination({
  team,
  kind,
  stream,
  canConfigure,
}: {
  team: string
  kind: StreamKind
  stream?: AuditStream
  canConfigure: boolean
}) {
  const time = useTimeFormat()
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [removing, setRemoving] = useState(false)
  const test = useMutation({
    mutationFn: () => testAuditStream(team, kind),
    onSettled: () => queryClient.invalidateQueries({ queryKey: auditStreamsQuery(team).queryKey }),
  })
  return (
    <Section
      title={kinds[kind].title}
      info={{ title: kinds[kind].title, body: kinds[kind].about }}
      action={
        canConfigure && (
          <span className="flex gap-2">
            {stream && (
              <Button variant="outline" disabled={test.isPending} onClick={() => test.mutate()}>
                {test.isPending ? "Sending…" : "Send test event"}
              </Button>
            )}
            <Button variant={stream ? "outline" : "default"} onClick={() => setEditing(true)}>
              {stream ? "Edit" : "Set up"}
            </Button>
          </span>
        )
      }
    >
      {!stream ? (
        <p className="py-3 text-sm text-muted-foreground">Not set up.</p>
      ) : (
        <>
          <Row label={kind === "s3" ? "Bucket" : "Endpoint"}>
            <span className="font-mono text-code">{kind === "s3" ? `${stream.bucket}/${stream.prefix ?? ""}` : stream.url}</span>
          </Row>
          <Row label="Status">
            {stream.failing_since ? (
              <span className="text-destructive">Failing since {time.dateTime(stream.failing_since)}</span>
            ) : stream.last_delivered_at ? (
              `Delivered ${time.relative(stream.last_delivered_at)}`
            ) : (
              "Waiting for the first event"
            )}
          </Row>
          {stream.pending > 0 && <Row label="Waiting to be sent">{stream.pending} events</Row>}
          {stream.last_error && (
            <p className="border-b border-divider py-3 text-sm text-destructive">Last error: {stream.last_error}</p>
          )}
          {test.isSuccess && <p className="py-2 text-sm text-muted-foreground">Test event delivered.</p>}
          {test.isError && <p className="py-2 text-sm text-destructive">{test.error.message}</p>}
          {canConfigure && (
            <Button variant="ghost" className="mt-2 self-start text-destructive" onClick={() => setRemoving(true)}>
              Stop streaming
            </Button>
          )}
        </>
      )}
      <StreamDialog team={team} kind={kind} stream={stream} open={editing} onOpenChange={setEditing} />
      <ConfirmAction
        open={removing}
        onOpenChange={setRemoving}
        title={`Stop streaming to ${kinds[kind].title}?`}
        description="Events stop being sent to this destination. The audit log itself is kept."
        action="Stop streaming"
        pending="Stopping…"
        mutation={{
          mutationFn: () => removeAuditStream(team, kind),
          onSuccess: () => queryClient.invalidateQueries({ queryKey: auditStreamsQuery(team).queryKey }),
        }}
      />
    </Section>
  )
}

type Draft = Required<AuditStreamInput>

function StreamDialog({
  team,
  kind,
  stream,
  open,
  onOpenChange,
}: {
  team: string
  kind: StreamKind
  stream?: AuditStream
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const queryClient = useQueryClient()
  const blank: Draft = {
    url: stream?.url ?? "",
    header_name: stream?.header_name ?? "",
    header_value: "",
    region: stream?.region ?? "",
    bucket: stream?.bucket ?? "",
    prefix: stream?.prefix ?? "",
    access_key_id: stream?.access_key_id ?? "",
    secret_access_key: "",
  }
  const [draft, setDraft] = useState(blank)
  const [secret, setSecret] = useState<string>()
  const keepsHeader = Boolean(stream?.header_name) && draft.header_name.trim() === stream?.header_name
  const save = useMutation({
    mutationFn: () => {
      const input: AuditStreamInput =
        kind === "https"
          ? {
              url: draft.url.trim(),
              // An empty name clears the header; an empty value keeps the stored one (PUT semantics).
              header_name: draft.header_name.trim(),
              ...(draft.header_value && { header_value: draft.header_value }),
            }
          : {
              url: draft.url.trim(),
              region: draft.region.trim(),
              bucket: draft.bucket.trim(),
              prefix: draft.prefix.trim(),
              access_key_id: draft.access_key_id.trim(),
              ...(draft.secret_access_key && { secret_access_key: draft.secret_access_key }),
            }
      return saveAuditStream(team, kind, input)
    },
    onSuccess: async (saved) => {
      await queryClient.invalidateQueries({ queryKey: auditStreamsQuery(team).queryKey })
      // The signing secret is returned once, when an HTTPS stream is created.
      if (saved.signing_secret) setSecret(saved.signing_secret)
      else onOpenChange(false)
    },
  })
  const field = (name: keyof Draft, label: string, props: React.ComponentProps<typeof Input> = {}) => (
    <Field className="gap-1.5">
      <FieldLabel htmlFor={`stream-${name}`}>{label}</FieldLabel>
      <Input
        id={`stream-${name}`}
        value={draft[name]}
        onChange={(event) => setDraft({ ...draft, [name]: event.target.value })}
        autoComplete="off"
        spellCheck={false}
        {...props}
      />
    </Field>
  )
  const close = (next: boolean) => {
    if (!next) {
      setSecret(undefined)
      setDraft(blank)
      save.reset()
    }
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="gap-4 sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{secret ? "Signing secret" : `${stream ? "Edit" : "Set up"} ${kinds[kind].title} streaming`}</DialogTitle>
          <DialogDescription>
            {secret
              ? "Every request carries Pb-Signature: t=<time>,v1=<HMAC-SHA256 of “t.body” with this secret>. Copy it now; it is not shown again."
              : kinds[kind].about}
          </DialogDescription>
        </DialogHeader>
        {secret ? (
          <>
            <CopyRow value={secret} className="font-mono text-code">
              {secret}
            </CopyRow>
            <DialogFooter>
              <Button onClick={() => close(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              save.mutate()
            }}
          >
            {kind === "https" ? (
              <>
                {field("url", "Endpoint", { type: "url", required: true, placeholder: "https://siem.example.com/ingest" })}
                {field("header_name", "Header name (optional)", { placeholder: "Authorization" })}
                {draft.header_name.trim() &&
                  // A header that is already stored keeps its value unless a new one is entered.
                  field("header_value", keepsHeader ? "Header value (leave empty to keep the current one)" : "Header value", {
                    type: "password",
                    required: !keepsHeader,
                  })}
              </>
            ) : (
              <>
                {field("url", "Endpoint", { type: "url", required: true, placeholder: "https://s3.eu-central-1.amazonaws.com" })}
                <div className="grid grid-cols-2 gap-3">
                  {field("region", "Region", { required: true })}
                  {field("bucket", "Bucket", { required: true })}
                </div>
                {field("prefix", "Prefix (optional)")}
                {field("access_key_id", "Access key ID", { required: true })}
                {field("secret_access_key", stream ? "Secret access key (leave empty to keep the current one)" : "Secret access key", {
                  type: "password",
                  required: !stream,
                })}
                <p className="text-xs text-muted-foreground">The key only needs permission to put objects under the prefix.</p>
              </>
            )}
            {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
            <DialogFooter>
              <Button type="submit" disabled={save.isPending}>
                {save.isPending ? "Checking…" : stream ? "Save" : "Start streaming"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}
