"use client"

import { WarningIcon } from "@phosphor-icons/react"
import { useMutation } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { useState } from "react"
import { useApiQuery } from "@/lib/api"
import {
  accountDeletionLive,
  deleteAccount,
  deletionPreviewQuery,
  DeletionError,
  type DeletionPreview,
} from "@/lib/account-deletion"
import { useTimeFormat } from "@/lib/use-time-format"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

const reauthPath = "/auth/sign-in?reauth=1&return_to=%2Faccount%2Fdelete"

/**
 * Deleting the account: exactly what goes (from the server's preview), then the account's email
 * typed out, then one red button. Owned teams with other members block it until they are handed
 * over or deleted. The server asks for a sign-in from the last five minutes.
 */
export function DeleteAccount() {
  const preview = useApiQuery(deletionPreviewQuery)
  if (!accountDeletionLive) return <p className="text-sm text-muted-foreground">Deleting an account isn’t available yet.</p>
  if (preview.isPending) return <Skeleton className="h-48 w-full" />
  if (preview.isError) return <p className="text-sm text-destructive">{preview.error.message}</p>
  const data = preview.data
  return (
    <>
      <WhatGoes preview={data} />
      {data.teams_blocking.length > 0 ? <Blocking teams={data.teams_blocking} /> : <Confirm preview={data} />}
    </>
  )
}

function WhatGoes({ preview }: { preview: DeletionPreview }) {
  const { date } = useTimeFormat()
  const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
  return (
    <Section title="What is deleted">
      <Row label="Machines">{count(preview.machines, "machine", "machines")} removed from Paperboat</Row>
      <Row label="Environment variables">{count(preview.env_vaults, "personal vault", "personal vaults")}</Row>
      <Row label="Previews and tunnels">
        {count(preview.previews, "preview", "previews")}, {count(preview.tunnels, "tunnel", "tunnels")}
      </Row>
      <Row label="Config sync">{preview.config_sync ? "Your config sync setup" : "Not set up"}</Row>
      <Row label="Inbox">{count(preview.inbox_items, "item", "items")}</Row>
      <Row label="API tokens">{count(preview.api_tokens, "token", "tokens")}</Row>
      <Row label="Subscription">
        {preview.subscription
          ? `${planLabel(preview.subscription.plan)} ends now${preview.subscription.period_end ? `, not at ${date(preview.subscription.period_end, "date")}` : ""}`
          : "None"}
      </Row>
      <Row label="Teams">
        {preview.teams_deleted.length
          ? `${preview.teams_deleted.map((team) => team.name).join(", ")} (you are the only member)`
          : "None"}
      </Row>
    </Section>
  )
}

/** A plan code from the preview ("pro-month") as people read it ("Pro, monthly"). */
function planLabel(code: string) {
  const [plan, interval] = code.split("-")
  const name = plan.charAt(0).toUpperCase() + plan.slice(1)
  return interval === "month" ? `${name}, monthly` : interval === "year" ? `${name}, yearly` : name
}

function Blocking({ teams }: { teams: DeletionPreview["teams_blocking"] }) {
  return (
    <div className="flex flex-col gap-2 rounded-md border border-destructive/40 p-4 text-sm" data-deletion-blocked>
      <p className="font-medium">First hand over or delete these teams</p>
      <p className="text-muted-foreground">You own them and they have other members, so they can’t be deleted with you.</p>
      <ul className="flex flex-col gap-1">
        {teams.map((team) => (
          <li key={team.slug}>
            <Link className="underline underline-offset-4" href={`/team?team=${encodeURIComponent(team.slug)}`}>
              {team.name}
            </Link>{" "}
            <span className="text-muted-foreground">· {team.members} members</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

function Confirm({ preview }: { preview: DeletionPreview }) {
  const router = useRouter()
  const [typed, setTyped] = useState("")
  const remove = useMutation({
    mutationFn: () => deleteAccount(typed.trim()),
    // The server ended the session; the deleted page needs none.
    onSuccess: () => router.replace("/auth/deleted"),
    onError: (error) => {
      // Sign-in is a route handler (a full page load), not a page the router can show.
      // eslint-disable-next-line @next/next/no-location-assign-relative-destination
      if (error instanceof DeletionError && error.code === "reauthentication_required") window.location.href = reauthPath
    },
  })
  if (preview.reauthentication_required) {
    return (
      <div className="flex flex-col items-start gap-3 text-sm">
        <p>To delete your account, sign in again first. You come back here afterwards.</p>
        <Button variant="outline" nativeButton={false} render={<a href={reauthPath} />}>
          Sign in again
        </Button>
      </div>
    )
  }
  const matches = typed.trim().toLowerCase() === preview.account_email.toLowerCase()
  const failure = remove.error instanceof DeletionError ? remove.error : undefined
  return (
    <form
      className="flex flex-col gap-3 text-sm"
      onSubmit={(event) => {
        event.preventDefault()
        if (matches) remove.mutate()
      }}
    >
      <p className="flex items-center gap-2 font-medium">
        <WarningIcon className="size-4 text-destructive" aria-hidden />
        This can’t be undone.
      </p>
      {preview.subscription && (
        <p className="text-muted-foreground">
          Your subscription ends now. No refund is given for the rest of the current period.
        </p>
      )}
      <label htmlFor="confirm-email" className="text-muted-foreground">
        Type <span className="font-medium text-foreground">{preview.account_email}</span> to confirm
      </label>
      <Input
        id="confirm-email"
        value={typed}
        onChange={(event) => setTyped(event.target.value)}
        autoComplete="off"
        spellCheck={false}
        className="max-w-sm"
      />
      {failure && <p className="text-destructive">{failure.message}</p>}
      {failure?.teams && <Blocking teams={failure.teams} />}
      <div>
        <Button
          type="submit"
          className="bg-destructive text-white hover:bg-destructive/90"
          disabled={!matches || remove.isPending}
        >
          {remove.isPending ? "Deleting…" : "Delete my account"}
        </Button>
      </div>
    </form>
  )
}
