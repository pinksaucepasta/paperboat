"use client"

import { CaretDownIcon, CheckIcon, CopyIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { useApiQuery, workspacesQuery } from "@/lib/api"
import { cn } from "@/lib/utils"
import { useNow } from "@/lib/use-now"
import { inviteText, rememberCode, terminalLink, useKnownCode } from "@/lib/terminal/invite"
import {
  changeCodeAccess,
  codeAccessQuery,
  policyQuery,
  savePolicy,
  terminalKeys,
  type AccessPolicy,
  type CodeAccess,
} from "@/lib/terminal/queries"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"
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
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { copyText } from "@/lib/clipboard"

const emailPattern = /^[^\s@,]+@[^\s@,]+\.[^\s@,]+$/
const domainPattern = /^\*@[a-z0-9-]+(\.[a-z0-9-]+)+$/
type Expiry = 3600 | 28800 | 86400
const expiryLabels: Record<Expiry, string> = { 3600: "1 hour", 28800: "8 hours", 86400: "24 hours" }

export function codeIsLive(access: CodeAccess | undefined, now: number) {
  return !!access?.enabled && !!access.expires_at && Date.parse(access.expires_at) > now
}

export function useCopy() {
  const [copied, setCopied] = useState<string | null>(null)
  return {
    copied,
    copy: (key: string, text: string, message?: string) =>
      void copyText(text).then((done) => {
        if (!done) return void toast.error("Couldn’t copy", { description: "Select it and copy it yourself." })
        setCopied(key)
        if (message) toast.success(message)
        setTimeout(() => setCopied((current) => (current === key ? null : current)), 1500)
      }),
  }
}

/**
 * Share works like a meeting invite. With no one named, the link and a code let anyone in, and
 * the code is ready when the dialog opens. Naming people (emails) or the team replaces the code:
 * they sign in instead. Every change applies at once.
 */
export function TerminalShareDialog({
  session,
  open,
  onOpenChange,
}: {
  session: TerminalSession
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="gap-5 sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Share {session.name}</DialogTitle>
          <DialogDescription>
            Everyone who joins sees this terminal live, including any secrets it prints.
          </DialogDescription>
        </DialogHeader>
        {open && <Share session={session} onDone={() => onOpenChange(false)} />}
      </DialogContent>
    </Dialog>
  )
}

type Audience = { audience: AccessPolicy["audience"]; emails: string[]; typists: string[] }

function Share({ session, onDone }: { session: TerminalSession; onDone: () => void }) {
  const queryClient = useQueryClient()
  const policy = useApiQuery(policyQuery(session.id))
  const access = useApiQuery(codeAccessQuery(session.id))
  const { data: workspaces } = useApiQuery(workspacesQuery)
  const now = useNow()
  const [confirm, setConfirm] = useState<{ who: string; apply: () => void } | null>(null)

  const save = useMutation({
    mutationFn: async (next: Audience) => {
      const current = queryClient.getQueryData<AccessPolicy>(terminalKeys.policy(session.id)) ?? policy.data!
      const updated = await savePolicy(session.id, current, {
        audience: next.audience,
        teamId: session.owner_team_id,
        emails: next.emails,
        interactive: next.typists,
      })
      queryClient.setQueryData(terminalKeys.policy(session.id), updated)
      // Named people sign in, so the code is switched off rather than left working.
      const code = queryClient.getQueryData<CodeAccess>(terminalKeys.code(session.id))
      if (next.audience !== "owner" && code?.enabled) {
        const off = await changeCodeAccess(session.id, code, { enabled: false, role: code.role, expiresIn: 3600 })
        queryClient.setQueryData(terminalKeys.code(session.id), off)
      }
    },
    onError: (error) => toast.error("Sharing wasn’t changed", { description: error.message }),
  })

  const stop = useMutation({
    mutationFn: async () => {
      const current = queryClient.getQueryData<AccessPolicy>(terminalKeys.policy(session.id))!
      if (current.audience !== "owner")
        queryClient.setQueryData(
          terminalKeys.policy(session.id),
          await savePolicy(session.id, current, { audience: "owner", emails: [], interactive: [] }),
        )
      const code = queryClient.getQueryData<CodeAccess>(terminalKeys.code(session.id))
      if (code?.enabled)
        queryClient.setQueryData(
          terminalKeys.code(session.id),
          await changeCodeAccess(session.id, code, { enabled: false, role: code.role, expiresIn: 3600 }),
        )
    },
    onSuccess: () => {
      toast.success("Only you can open this terminal now")
      onDone()
    },
  })

  if (policy.isPending || access.isPending) return <Skeleton className="h-40" />
  if (policy.isError || access.isError) return <p className="text-sm text-destructive">Couldn’t load sharing.</p>

  // While a change is saving, show it as made.
  const saved = policy.data
  const shown: Audience = save.isPending
    ? save.variables
    : {
        audience: saved.audience,
        emails: saved.audience === "specific_emails" ? saved.allowed_emails : [],
        typists: saved.audience === "specific_emails" ? saved.interactive_emails : [],
      }
  const team = session.owner_team_id
    ? (workspaces?.find((workspace) => workspace.id === session.owner_team_id)?.name ?? "your team")
    : undefined

  const setPeople = (emails: string[], typists: string[]) =>
    save.mutate({ audience: emails.length ? "specific_emails" : "owner", emails, typists })

  return (
    <div className="flex min-w-0 flex-col gap-5">
      {team && (
        <label className="flex items-center justify-between gap-4 text-sm">
          <span>
            Everyone in {team} can join
            <span className="block text-xs text-muted-foreground">They can watch. Only you can type.</span>
          </span>
          <Switch
            checked={shown.audience === "team"}
            disabled={save.isPending}
            onCheckedChange={(on) => save.mutate({ audience: on ? "team" : "owner", emails: [], typists: [] })}
          />
        </label>
      )}

      {shown.audience !== "team" && (
        <div className="flex min-w-0 flex-col gap-3">
          <EmailField
            disabled={save.isPending}
            onAdd={(added) =>
              setPeople([...shown.emails, ...added.filter((email) => !shown.emails.includes(email))], shown.typists)
            }
          />
          <p className="-mt-1 text-xs text-muted-foreground">
            {shown.emails.length
              ? "Only these people can join, after signing in."
              : "Leave this empty to let anyone with the link and code join."}
          </p>

          {shown.emails.length > 0 ? (
            <ul className="-mx-2 flex max-h-56 flex-col overflow-y-auto">
              {shown.emails.map((email) => {
                const domain = email.startsWith("*@")
                const types = shown.typists.includes(email)
                return (
                  <li key={email} className="flex h-10 min-w-0 items-center gap-3 rounded-md px-2 text-sm">
                    <span className="min-w-0 flex-1 truncate">{domain ? `Everyone at ${email.slice(2)}` : email}</span>
                    <RoleMenu
                      value={types ? "interactive" : "viewer"}
                      disabled={domain || save.isPending}
                      onChange={(role) => {
                        const typists =
                          role === "interactive"
                            ? [...shown.typists, email]
                            : shown.typists.filter((entry) => entry !== email)
                        if (role === "interactive")
                          setConfirm({ who: email, apply: () => setPeople(shown.emails, typists) })
                        else setPeople(shown.emails, typists)
                      }}
                    />
                    <Button
                      size="icon-sm"
                      variant="ghost"
                      aria-label={`Remove ${email}`}
                      className="text-muted-foreground"
                      disabled={save.isPending}
                      onClick={() =>
                        setPeople(
                          shown.emails.filter((entry) => entry !== email),
                          shown.typists.filter((entry) => entry !== email),
                        )
                      }
                    >
                      <XIcon />
                    </Button>
                  </li>
                )
              })}
            </ul>
          ) : (
            <JoiningInfo session={session} access={access.data} now={now} onConfirm={setConfirm} />
          )}
        </div>
      )}

      <div className="flex items-center justify-between gap-2 border-t pt-4">
        {shown.audience !== "owner" || codeIsLive(access.data, now) ? (
          <Button
            variant="ghost"
            className="-ml-2.5 text-destructive"
            disabled={stop.isPending}
            onClick={() => stop.mutate()}
          >
            {stop.isPending ? "Stopping…" : "Stop sharing"}
          </Button>
        ) : (
          <span />
        )}
        <div className="flex gap-2">
          {shown.audience !== "owner" && <InviteButton session={session} />}
          <Button variant="secondary" onClick={onDone}>
            Done
          </Button>
        </div>
      </div>

      <AlertDialog open={!!confirm} onOpenChange={(next) => !next && setConfirm(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Let them type?</AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.who} will be able to run commands on {session.target.name} as you.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                confirm?.apply()
                setConfirm(null)
              }}
            >
              Allow typing
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

/** Enter, comma, space or Tab adds what's typed (several at once when pasted); the list below shows them. */
function EmailField({ disabled, onAdd }: { disabled: boolean; onAdd: (emails: string[]) => void }) {
  const [draft, setDraft] = useState("")
  const [error, setError] = useState("")
  const inputRef = useRef<HTMLInputElement>(null)

  const commit = (text: string) => {
    const parts = text
      .split(/[\s,;]+/)
      .map((part) => part.trim().toLowerCase())
      .filter(Boolean)
    if (!parts.length) {
      setDraft("")
      return
    }
    const bad = parts.find((part) => !emailPattern.test(part) || (part.startsWith("*@") && !domainPattern.test(part)))
    if (bad) {
      setDraft(text.trim())
      setError(`“${bad}” isn’t an email. Use name@company.com, or *@company.com for a whole domain.`)
      return
    }
    setError("")
    setDraft("")
    onAdd(parts)
  }

  return (
    <div className="flex flex-col gap-1">
      <div
        onClick={() => inputRef.current?.focus()}
        className={cn(
          "flex h-9 w-full cursor-text items-center rounded-md border border-input bg-transparent px-3 text-sm transition-colors focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/30 dark:bg-input/30",
          error && "border-destructive",
        )}
      >
        <input
          ref={inputRef}
          value={draft}
          // Read-only, not disabled, while a change saves: the field keeps focus for the next email.
          readOnly={disabled}
          onChange={(event) => {
            const value = event.target.value
            // A separator typed ends an email.
            if (/[\s,;]$/.test(value)) commit(value)
            else {
              setDraft(value)
              if (error) setError("")
            }
          }}
          onKeyDown={(event) => {
            if ((event.key === "Enter" || event.key === "Tab") && draft.trim()) {
              event.preventDefault()
              commit(draft)
            }
          }}
          onBlur={() => draft.trim() && commit(draft)}
          onPaste={(event) => {
            const text = event.clipboardData.getData("text")
            if (/[\s,;]/.test(text.trim())) {
              event.preventDefault()
              commit(draft + text)
            }
          }}
          placeholder="Add people by email"
          autoComplete="off"
          spellCheck={false}
          className="h-6 min-w-32 flex-1 bg-transparent outline-none placeholder:text-muted-foreground"
        />
      </div>
      {error && <p className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

function RoleMenu({
  value,
  disabled,
  onChange,
}: {
  value: CodeAccess["role"]
  disabled?: boolean
  onChange: (role: CodeAccess["role"]) => void
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={disabled}
        className="inline-flex items-center gap-1 rounded-sm text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none"
      >
        {value === "interactive" ? "can type" : "can watch"}
        {!disabled && <CaretDownIcon className="size-3" />}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-36">
        <DropdownMenuRadioGroup value={value} onValueChange={(next) => onChange(next as CodeAccess["role"])}>
          <DropdownMenuRadioItem value="viewer">Can watch</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="interactive">Can type</DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/**
 * The link and code, ready when Share opens: a code is made if there's none, and one made in
 * another browser (which can't be shown here) is replaced, unless guests are using it.
 */
function JoiningInfo({
  session,
  access,
  now,
  onConfirm,
}: {
  session: TerminalSession
  access: CodeAccess
  now: number
  onConfirm: (confirm: { who: string; apply: () => void }) => void
}) {
  const queryClient = useQueryClient()
  const live = codeIsLive(access, now)
  const code = useKnownCode(session.id, access)
  const [expiresIn, setExpiresIn] = useState<Expiry>(28800)
  const { copied, copy } = useCopy()

  const change = useMutation({
    mutationFn: (next: { reset?: boolean; role?: CodeAccess["role"]; expiresIn?: Expiry; confirm?: boolean }) => {
      const current = queryClient.getQueryData<CodeAccess>(terminalKeys.code(session.id)) ?? access
      return changeCodeAccess(session.id, current, {
        enabled: true,
        reset: next.reset,
        role: next.role ?? current.role,
        expiresIn: next.expiresIn ?? expiresIn,
        confirmControl: next.confirm,
      })
    },
    onSuccess: (updated) => {
      // Cache first, so nothing renders the new code against the old state.
      queryClient.setQueryData(terminalKeys.code(session.id), { ...updated, code: undefined })
      if (updated.code) rememberCode(session.id, updated.code, updated.generation, updated.expires_at)
    },
  })

  // Ready on open: make a code, or replace an unshowable one nobody is using. Once per dialog.
  const prepared = useRef(false)
  useEffect(() => {
    if (prepared.current || now === 0) return
    if (!live) {
      prepared.current = true
      change.mutate({ role: "viewer" })
    } else if (!code && access.guests.length === 0) {
      prepared.current = true
      change.mutate({ reset: true })
    }
  }, [live, code, access.guests.length, now, change])

  const guests = access.guests.length
  // A new role or expiry means a new code (the server rotates it), which drops current guests.
  const rotate = (next: { role?: CodeAccess["role"]; expiresIn?: Expiry }) => {
    if (next.expiresIn) setExpiresIn(next.expiresIn)
    const role = next.role ?? access.role
    const apply = () => change.mutate({ reset: true, role, expiresIn: next.expiresIn, confirm: role === "interactive" })
    if (role === "interactive" && access.role !== "interactive") onConfirm({ who: "Anyone with the code", apply })
    else apply()
  }

  const hours = live ? Math.round((Date.parse(access.expires_at!) - now) / 3_600_000) : 0
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-lg bg-surface px-4 py-3">
      <Field
        label="Link"
        value={terminalLink(session.id, true)}
        copied={copied === "link"}
        onCopy={() => copy("link", terminalLink(session.id, true))}
      />
      <div className="flex h-10 min-w-0 items-center gap-3">
        <span className="w-10 shrink-0 text-xs text-muted-foreground">Code</span>
        {code && live ? (
          <>
            <span className="min-w-0 flex-1 truncate font-mono text-lg tracking-[0.2em]">{code}</span>
            <Button size="icon-sm" variant="ghost" aria-label="Copy code" onClick={() => copy("code", code)}>
              {copied === "code" ? <CheckIcon /> : <CopyIcon />}
            </Button>
          </>
        ) : change.isError ? (
          <span className="min-w-0 flex-1 truncate text-xs text-destructive">{change.error.message}</span>
        ) : live && guests > 0 ? (
          <>
            <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
              Made in another browser and in use by {guests === 1 ? "1 guest" : `${guests} guests`}
            </span>
            <Button size="sm" variant="ghost" onClick={() => change.mutate({ reset: true })}>
              New code
            </Button>
          </>
        ) : (
          <Skeleton className="h-5 w-28" />
        )}
      </div>
      <div className="flex flex-wrap items-center gap-x-1 text-xs text-muted-foreground">
        Guests
        <RoleMenu value={access.role} disabled={change.isPending} onChange={(role) => rotate({ role })} />· code works
        for
        <DropdownMenu>
          <DropdownMenuTrigger
            disabled={change.isPending}
            className="inline-flex items-center gap-1 rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
          >
            {hours > 0 ? (hours === 1 ? "about an hour" : `${hours} more hours`) : expiryLabels[expiresIn]}
            <CaretDownIcon className="size-3" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-36">
            <DropdownMenuRadioGroup
              value={String(expiresIn)}
              onValueChange={(value) => rotate({ expiresIn: Number(value) as Expiry })}
            >
              {([3600, 28800, 86400] as const).map((value) => (
                <DropdownMenuRadioItem key={value} value={String(value)}>
                  {expiryLabels[value]}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      {guests > 0 && (
        <p className="text-xs text-muted-foreground">Changing either makes a new code and disconnects guests.</p>
      )}
      <Button
        className="mt-3 self-start"
        disabled={!code || !live}
        onClick={() => copy("invite", inviteText(session, code, access.expires_at), "Invite copied")}
      >
        {copied === "invite" ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
        Copy invite
      </Button>
    </div>
  )
}

function Field({
  label,
  value,
  copied,
  onCopy,
}: {
  label: string
  value: string
  copied: boolean
  onCopy: () => void
}) {
  return (
    <div className="flex h-8 min-w-0 items-center gap-3">
      <span className="w-10 shrink-0 text-xs text-muted-foreground">{label}</span>
      <span className="min-w-0 flex-1 truncate font-mono text-xs">{value}</span>
      <Button size="icon-sm" variant="ghost" aria-label={`Copy ${label.toLowerCase()}`} onClick={onCopy}>
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>
    </div>
  )
}

/** Named people and teams sign in, so the link is all their invite needs. */
function InviteButton({ session }: { session: TerminalSession }) {
  const { copied, copy } = useCopy()
  return (
    <Button onClick={() => copy("invite", inviteText(session), "Invite copied")}>
      {copied === "invite" ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
      Copy invite
    </Button>
  )
}
