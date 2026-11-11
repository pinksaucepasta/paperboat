"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { ApiError, useApiQuery } from "@/lib/api"
import {
  bucketMessage,
  removeTeamBucket,
  saveTeamBucket,
  teamBucketQuery,
  type TeamBucket,
} from "@/lib/team-bucket"
import { useTimeFormat } from "@/lib/use-time-format"
import { ConfirmAction } from "@/components/list-toolbar"
import { Row, Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"

/**
 * Team → Storage: the team's own S3-compatible bucket, where machines archive their terminal recordings. Owners and admins
 * change it. The access key is stored encrypted and never shown; saving checks the bucket and names the step that fails.
 */
export function TeamStorage({ team, admin }: { team: string; admin: boolean }) {
  const bucket = useApiQuery(teamBucketQuery(team))
  const [editing, setEditing] = useState(false)
  const [removing, setRemoving] = useState(false)
  const queryClient = useQueryClient()
  if (bucket.error instanceof ApiError && bucket.error.code === "team_plan_required")
    return (
      <p className="rounded-lg bg-surface px-4 py-3 text-sm text-muted-foreground" data-storage-upgrade>
        The team bucket is part of the Teams plan.{" "}
        <Link href="/billing" className="text-foreground underline underline-offset-4">
          Upgrade
        </Link>
      </p>
    )
  if (bucket.isError) return <p className="text-sm text-muted-foreground">{bucketMessage(bucket.error)}</p>
  return (
    <div className="flex flex-col gap-10" data-team-storage>
      <Section
        title="Bucket"
        action={
          admin && bucket.data !== undefined ? (
            <Button size="lg" variant={bucket.data ? "outline" : "default"} onClick={() => setEditing(true)}>
              {bucket.data ? "Edit" : "Connect"}
            </Button>
          ) : undefined
        }
      >
        {bucket.data === undefined ? (
          <Row label="Bucket">
            <Skeleton className="my-0.5 h-4 w-40" />
          </Row>
        ) : bucket.data === null ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground" data-storage-empty>
            No bucket connected.
          </p>
        ) : (
          <BucketRows bucket={bucket.data} />
        )}
      </Section>
      {bucket.data && admin && (
        <Button variant="ghost" className="self-start text-destructive" onClick={() => setRemoving(true)}>
          Disconnect bucket
        </Button>
      )}
      {editing && <BucketDialog team={team} bucket={bucket.data ?? undefined} onClose={() => setEditing(false)} />}
      <ConfirmAction
        open={removing}
        onOpenChange={setRemoving}
        title="Disconnect the bucket?"
        description="Recordings already in the bucket stay there, and recordings stay on their machines."
        action="Disconnect"
        pending="Disconnecting…"
        mutation={{
          mutationFn: () => removeTeamBucket(team),
          onSuccess: () => queryClient.invalidateQueries({ queryKey: teamBucketQuery(team).queryKey }),
        }}
      />
    </div>
  )
}

function BucketRows({ bucket }: { bucket: TeamBucket }) {
  const time = useTimeFormat()
  return (
    <>
      <Row label="Bucket">
        <span className="font-mono text-code" data-storage-bucket>
          {bucket.bucket}
          {bucket.prefix && `/${bucket.prefix}`}
        </span>
      </Row>
      <Row label="Endpoint">
        <span className="font-mono text-code">{bucket.endpoint}</span>
      </Row>
      <Row label="Access key ID">
        <span className="font-mono text-code">{bucket.access_key_id}</span>
      </Row>
      <Row label="Checked">{time.relative(bucket.checked_at)}</Row>
    </>
  )
}

function BucketDialog({ team, bucket, onClose }: { team: string; bucket?: TeamBucket; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState({
    endpoint: bucket?.endpoint ?? "",
    region: bucket?.region ?? "",
    bucket: bucket?.bucket ?? "",
    prefix: bucket?.prefix ?? "",
    access_key_id: bucket?.access_key_id ?? "",
    secret_access_key: "",
  })
  const [pathStyle, setPathStyle] = useState(bucket?.path_style ?? false)
  const save = useMutation({
    mutationFn: () =>
      saveTeamBucket(team, {
        endpoint: draft.endpoint.trim(),
        region: draft.region.trim(),
        bucket: draft.bucket.trim(),
        prefix: draft.prefix.trim(),
        access_key_id: draft.access_key_id.trim(),
        secret_access_key: draft.secret_access_key,
        path_style: pathStyle,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: teamBucketQuery(team).queryKey })
      onClose()
    },
  })
  const field = (name: keyof typeof draft, label: string, props: React.ComponentProps<typeof Input> = {}) => (
    <Field className="gap-1.5">
      <FieldLabel htmlFor={`bucket-${name}`}>{label}</FieldLabel>
      <Input
        id={`bucket-${name}`}
        value={draft[name]}
        onChange={(event) => setDraft({ ...draft, [name]: event.target.value })}
        autoComplete="off"
        spellCheck={false}
        {...props}
      />
    </Field>
  )
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{bucket ? "Edit bucket" : "Connect a bucket"}</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            save.mutate()
          }}
        >
          {field("endpoint", "Endpoint", { type: "url", required: true, placeholder: "https://s3.eu-central-1.amazonaws.com" })}
          <div className="grid grid-cols-2 gap-3">
            {field("region", "Region", { required: true })}
            {field("bucket", "Bucket", { required: true })}
          </div>
          {field("prefix", "Folder (optional)")}
          {field("access_key_id", "Access key ID", { required: true })}
          {field("secret_access_key", bucket ? "Secret access key (leave empty to keep the current one)" : "Secret access key", {
            type: "password",
            required: !bucket,
          })}
          <SettingRow label="Address the bucket in the URL path" htmlFor="bucket-path-style">
            <Switch id="bucket-path-style" checked={pathStyle} onCheckedChange={setPathStyle} />
          </SettingRow>
          <p className="text-xs text-muted-foreground">The key needs permission to put, get and delete objects.</p>
          {save.isError && <p className="text-sm text-destructive">{bucketMessage(save.error)}</p>}
          <DialogFooter>
            <Button type="submit" size="lg" disabled={save.isPending}>
              {save.isPending ? "Checking…" : bucket ? "Save" : "Connect"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
