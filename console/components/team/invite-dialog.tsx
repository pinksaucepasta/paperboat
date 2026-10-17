"use client"

import { CheckCircleIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { api, ApiError } from "@/lib/api"
import { notLiveTitle } from "@/lib/billing"
import { inviteMember, teamInvitationsQuery, teamLive, teamQuery, type Team, type TeamRole } from "@/lib/teams"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Textarea } from "@/components/ui/textarea"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const maxInvites = 20

/** Emails pasted or typed, split on commas, spaces and new lines, without duplicates. */
function parseEmails(text: string) {
  return [
    ...new Set(
      text
        .split(/[\s,;]+/)
        .map((part) => part.trim().toLowerCase())
        .filter(Boolean),
    ),
  ]
}

type Outcome = { email: string; error?: ApiError | Error }

/**
 * Invite several people at once, as Vercel and Cloudflare do. Each invite changes the team's
 * generation, so they are sent one after another, each against the team as it now is.
 */
export function InviteDialog({
  team,
  owner,
  freeSeats,
  open,
  onOpenChange,
}: {
  team: Team
  owner: boolean
  /** Seats not used by members or pending invitations; undefined when the server does not say. */
  freeSeats?: number
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Invite members</DialogTitle>
        </DialogHeader>
        {open && <InviteForm team={team} owner={owner} freeSeats={freeSeats} onDone={() => onOpenChange(false)} />}
      </DialogContent>
    </Dialog>
  )
}

function InviteForm({
  team,
  owner,
  freeSeats,
  onDone,
}: {
  team: Team
  owner: boolean
  freeSeats?: number
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  const [text, setText] = useState("")
  const [role, setRole] = useState<TeamRole>("member")
  const [outcomes, setOutcomes] = useState<Outcome[]>([])
  const emails = parseEmails(text)
  const invalid = emails.filter((email) => !emailPattern.test(email))
  const send = useMutation({
    mutationFn: async () => {
      const results: Outcome[] = []
      for (const email of emails) {
        try {
          const current = await api<Team>(`teams/${encodeURIComponent(team.team_id)}`)
          await inviteMember(current, email, role)
          results.push({ email })
        } catch (error) {
          results.push({ email, error: error as Error })
        }
      }
      return results
    },
    onSuccess: async (results) => {
      await queryClient.invalidateQueries({ queryKey: teamQuery(team.team_id).queryKey })
      await queryClient.invalidateQueries({ queryKey: teamInvitationsQuery(team.team_id).queryKey })
      if (results.every((result) => !result.error)) return onDone()
      setOutcomes(results)
      setText(
        results
          .filter((result) => result.error)
          .map((result) => result.email)
          .join("\n"),
      )
    },
  })
  const seatShort = freeSeats !== undefined && emails.length > freeSeats
  const seatRequired = outcomes.some((o) => o.error instanceof ApiError && o.error.code === "seat_required")
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (emails.length && !invalid.length) send.mutate()
      }}
    >
      <Field className="gap-1.5">
        <FieldLabel htmlFor="invite-emails">Email addresses</FieldLabel>
        <Textarea
          id="invite-emails"
          autoFocus
          rows={3}
          placeholder="ana@company.com, sam@company.com"
          value={text}
          onChange={(event) => setText(event.target.value)}
          spellCheck={false}
        />
        <span className={invalid.length ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
          {invalid.length
            ? `Not an email: ${invalid.join(", ")}`
            : "Separate with commas or new lines. They join after signing in with that address."}
        </span>
      </Field>
      <Field className="gap-1.5">
        <FieldLabel>Role</FieldLabel>
        <ToggleGroup
          value={[role]}
          onValueChange={(next) => next[0] && setRole(next[0] as TeamRole)}
          variant="outline"
          spacing={0}
        >
          <ToggleGroupItem value="member">Member</ToggleGroupItem>
          <ToggleGroupItem
            value="admin"
            disabled={!owner || !teamLive.inviteRole}
            title={!teamLive.inviteRole ? `${notLiveTitle}: invite, then make them admin` : undefined}
          >
            Admin
          </ToggleGroupItem>
        </ToggleGroup>
        <span className="text-xs text-muted-foreground">
          {role === "admin"
            ? "Admins invite and remove members and manage machines."
            : "Members use the machines, ENV and config the team gives them."}
        </span>
      </Field>
      {freeSeats !== undefined && (
        <p className={seatShort ? "text-sm text-destructive" : "text-sm text-muted-foreground"}>
          {seatShort
            ? `${emails.length} invitations need ${emails.length - freeSeats} more ${emails.length - freeSeats === 1 ? "seat" : "seats"}.`
            : `${freeSeats} free ${freeSeats === 1 ? "seat" : "seats"}. Each invitation holds one until it is accepted or cancelled.`}
        </p>
      )}
      {outcomes.length > 0 && (
        <ul className="flex flex-col gap-1.5 text-sm">
          {outcomes.map(({ email, error }) => (
            <li key={email} className="flex items-start gap-2">
              {error ? (
                <WarningCircleIcon className="mt-0.5 size-4 shrink-0 text-destructive" />
              ) : (
                <CheckCircleIcon className="mt-0.5 size-4 shrink-0 text-success" />
              )}
              <span className="min-w-0">
                {email}
                {error && <span className="block text-muted-foreground">{error.message}</span>}
              </span>
            </li>
          ))}
        </ul>
      )}
      <DialogFooter>
        {seatShort || seatRequired ? (
          <Button size="lg" render={<Link href="/billing" onClick={onDone} />}>
            Add seats
          </Button>
        ) : null}
        <Button
          type="submit"
          size="lg"
          disabled={!emails.length || invalid.length > 0 || emails.length > maxInvites || send.isPending}
        >
          {send.isPending ? "Sending…" : emails.length > 1 ? `Send ${emails.length} invitations` : "Send invitation"}
        </Button>
      </DialogFooter>
    </form>
  )
}
