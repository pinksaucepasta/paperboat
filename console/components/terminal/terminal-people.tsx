"use client"

import { DotsThreeIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { cn } from "@/lib/utils"
import { removeParticipant, terminalKeys, type Participant } from "@/lib/terminal/queries"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"
import { StatusDot } from "@/components/machine-status"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"

export const roleLabels = { owner: "Owner", interactive: "Can type", viewer: "Watching" } as const

/** One row per person: two tabs of the same account are one participant with two windows. */
type Person = { accountId: string; role: Participant["role"]; windows: number; since: string }

export function peopleIn(session: TerminalSession): Person[] {
  const byAccount = new Map<string, Person>()
  for (const participant of session.participants) {
    const existing = byAccount.get(participant.account_id)
    if (existing) {
      existing.windows++
      if (participant.connected_at < existing.since) existing.since = participant.connected_at
    } else {
      byAccount.set(participant.account_id, {
        accountId: participant.account_id,
        role: participant.role,
        windows: 1,
        since: participant.connected_at,
      })
    }
  }
  // Owner first, then whoever has been here longest.
  return [...byAccount.values()].sort(
    (a, b) => Number(b.role === "owner") - Number(a.role === "owner") || a.since.localeCompare(b.since),
  )
}

/**
 * The server identifies participants by account id only (NOTES.md: Terminals), so people other
 * than you and the owner show a short, stable tag until it sends names.
 */
export function personName(accountId: string, session: TerminalSession, meId: string | undefined) {
  if (accountId === meId) return "You"
  if (accountId === session.owner_account) return "Owner"
  return `Participant ${accountId.slice(-4)}`
}

/** A letter tile, the same as the workspace switcher's, so people look like the rest of the console. */
export function PersonAvatar({ name, className }: { accountId?: string; name: string; className?: string }) {
  const letter = name === "You" || name === "Owner" ? name[0] : name.at(-1)?.toUpperCase()
  return (
    <span
      aria-hidden
      className={cn(
        "flex size-6 shrink-0 items-center justify-center rounded-md bg-primary/12 text-[11px] font-semibold text-primary",
        className,
      )}
    >
      {letter}
    </span>
  )
}

/** The side panel of a terminal: who's here and what they can do. Sharing lives in Share. */
export function TerminalPeople({
  session,
  meId,
  onClose,
}: {
  session: TerminalSession
  /** Undefined for guests, who have no account. */
  meId: string | undefined
  onClose: () => void
}) {
  const isOwner = session.role === "owner"
  const people = peopleIn(session)
  const [removing, setRemoving] = useState<Person | null>(null)

  return (
    <aside className="flex w-80 shrink-0 flex-col border-l bg-background text-sm" aria-label="People">
      <div className="flex h-11 shrink-0 items-center gap-2 border-b border-divider pr-2 pl-4">
        <h2 className="flex-1 font-medium">
          People <span className="font-normal text-muted-foreground tabular-nums">{people.length || ""}</span>
        </h2>
        <Button size="icon-sm" variant="ghost" aria-label="Close people" onClick={onClose}>
          <XIcon />
        </Button>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {!session.participants_available ? (
          <p className="px-4 py-3 text-muted-foreground">The machine isn’t reporting who’s here right now.</p>
        ) : (
          <ul className="divide-y divide-divider border-b border-divider">
            {people.map((person) => {
              const name = personName(person.accountId, session, meId)
              const removable = isOwner && person.accountId !== session.owner_account
              return (
                <li
                  key={person.accountId}
                  className="group flex h-14 items-center gap-3 px-4 transition-colors hover:bg-surface has-data-popup-open:bg-surface"
                >
                  <PersonAvatar name={name} />
                  <span className="flex min-w-0 flex-1 flex-col">
                    <span className="flex h-[1lh] items-center gap-2">
                      <span className="truncate-x font-medium trim-cap">{name}</span>
                      <StatusDot online />
                    </span>
                    <span className="truncate text-xs text-muted-foreground">
                      {roleLabels[person.role]}
                      {person.windows > 1 && ` · ${person.windows} windows`}
                    </span>
                  </span>
                  {removable && (
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Actions for ${name}`}
                            className="text-muted-foreground opacity-0 group-hover:opacity-100 hover:text-foreground focus-visible:opacity-100 data-popup-open:opacity-100"
                          />
                        }
                      >
                        <DotsThreeIcon weight="bold" className="size-4" />
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="w-48">
                        <DropdownMenuItem variant="destructive" onClick={() => setRemoving(person)}>
                          Remove from terminal
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  )}
                </li>
              )
            })}
          </ul>
        )}
        {session.participants_available && people.length <= 1 && (
          <p className="px-4 py-3 text-muted-foreground">No one else is here yet.</p>
        )}
      </div>

      {removing && (
        <RemoveParticipant
          session={session}
          person={removing}
          name={personName(removing.accountId, session, meId)}
          onClose={() => setRemoving(null)}
        />
      )}
    </aside>
  )
}

function RemoveParticipant({
  session,
  person,
  name,
  onClose,
}: {
  session: TerminalSession
  person: Person
  name: string
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const remove = useMutation({
    mutationFn: () => removeParticipant(session, person.accountId),
    onSuccess: (updated) => {
      queryClient.setQueryData(terminalKeys.session(session.id), updated)
      toast.success(`${name} removed`)
      onClose()
    },
  })
  return (
    <AlertDialog open onOpenChange={(next) => !next && !remove.isPending && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove {name}?</AlertDialogTitle>
          <AlertDialogDescription>
            They’re disconnected now and can’t rejoin until you share this terminal with them again.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {remove.isError && <p className="text-sm text-destructive">{remove.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={remove.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={remove.isPending}
            onClick={(event) => {
              event.preventDefault()
              remove.mutate()
            }}
          >
            {remove.isPending ? "Removing…" : "Remove"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
