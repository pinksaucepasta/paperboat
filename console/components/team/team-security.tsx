"use client"

import { ArrowSquareOutIcon, CheckCircleIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { ApiError, useApiQuery } from "@/lib/api"
import {
  identityQuery,
  openPortal,
  providerName,
  setExempt,
  updateDirectory,
  updateSso,
  type PortalIntent,
  type SsoChange,
  type SsoReview,
  type TeamIdentity,
} from "@/lib/identity"
import { roleLabels, type Team } from "@/lib/teams"
import { useTimeFormat } from "@/lib/use-time-format"
import { Row, Section } from "@/components/section"
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
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"

/**
 * Team → Security: verified domains, single sign-on (require it, session length, sign-up on first
 * sign-in, exemptions) and directory sync (which groups give which role). Setting up the identity
 * provider happens in WorkOS's Admin Portal, opened from here; it returns to this tab.
 */
export function TeamSecurity({ team, emails }: { team: Team; emails: Map<string, string> }) {
  const identity = useApiQuery(identityQuery(team.team_id))
  if (identity.isPending) return <Skeleton className="h-64 w-full" />
  if (identity.isError) return <p className="text-sm text-muted-foreground">{identity.error.message}</p>
  const data = identity.data
  if (!data.eligible) {
    return (
      <p className="rounded-lg bg-surface px-4 py-3 text-sm text-muted-foreground">
        Single sign-on and directory sync are available from {data.min_seats} seats. This team has {data.seats}.{" "}
        <Link href="/billing" className="text-foreground underline underline-offset-4">
          Add seats
        </Link>
      </p>
    )
  }
  return (
    <div className="flex flex-col gap-10">
      <Domains team={team} identity={data} />
      <SingleSignOn team={team} identity={data} emails={emails} />
      <Directory team={team} identity={data} />
    </div>
  )
}

function usePortal(team: Team) {
  return useMutation({
    mutationFn: (intent: PortalIntent) => openPortal(team.team_id, intent),
    // The Admin Portal is WorkOS's page; a new tab keeps the console where it was.
    onSuccess: ({ url }) => window.open(url, "_blank", "noopener"),
  })
}

function useSaveIdentity(team: Team) {
  const queryClient = useQueryClient()
  return (next: TeamIdentity) => queryClient.setQueryData(identityQuery(team.team_id).queryKey, next)
}

function PortalButton({ team, intent, children }: { team: Team; intent: PortalIntent; children: React.ReactNode }) {
  const portal = usePortal(team)
  return (
    <span className="flex items-center gap-2">
      {portal.isError && <span className="text-xs text-destructive">{portal.error.message}</span>}
      <Button variant="outline" disabled={portal.isPending} onClick={() => portal.mutate(intent)}>
        {children}
        <ArrowSquareOutIcon data-icon="inline-end" />
      </Button>
    </span>
  )
}

function State({ ok, children }: { ok: boolean; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      {ok ? (
        <CheckCircleIcon weight="fill" className="size-4 text-success" />
      ) : (
        <WarningCircleIcon weight="fill" className="size-4 text-amber-500" />
      )}
      {children}
    </span>
  )
}

const ready = (state: string) => state === "active" || state === "verified" || state === "linked"
const domainStates: Record<string, string> = { verified: "Verified", pending: "Waiting for DNS", failed: "Couldn’t be verified" }

function Domains({ team, identity }: { team: Team; identity: TeamIdentity }) {
  const domains = identity.sso.domains
  return (
    <Section
      title="Domains"
      info={{
        title: "Verified domains",
        body: "People with an email at a verified domain sign in through your identity provider.",
      }}
      action={identity.can_manage && <PortalButton team={team} intent="domain_verification">Verify a domain</PortalButton>}
    >
      {domains.length ? (
        domains.map((domain) => (
          <Row key={domain.domain} label={domain.domain}>
            <State ok={ready(domain.state)}>{domainStates[domain.state] ?? domain.state}</State>
          </Row>
        ))
      ) : (
        <p className="py-3 text-sm text-muted-foreground">No verified domains yet.</p>
      )}
    </Section>
  )
}

const hour = 3600
const sessionChoices = [hour, 8 * hour, 24 * hour, 7 * 24 * hour, 30 * 24 * hour]
const sessionLabel = (seconds: number) =>
  seconds % (24 * hour) === 0
    ? `${seconds / (24 * hour)} ${seconds === 24 * hour ? "day" : "days"}`
    : `${seconds / hour} ${seconds === hour ? "hour" : "hours"}`

function SingleSignOn({ team, identity, emails }: { team: Team; identity: TeamIdentity; emails: Map<string, string> }) {
  const save = useSaveIdentity(team)
  const sso = identity.sso
  const [review, setReview] = useState<SsoReview>()
  const change = useMutation({
    mutationFn: (next: SsoChange) => updateSso(team.team_id, next),
    onSuccess: (next) => {
      setReview(undefined)
      save(next)
    },
    onError: (error) => {
      // Requiring SSO first lists who would lose access; the same request with the digest confirms.
      if (error instanceof ApiError && error.code === "sso_review_required") setReview(error.details as SsoReview)
    },
  })
  const connected = sso.connections.some((connection) => ready(connection.state))
  const sessions = [...new Set([...sessionChoices, sso.session_seconds])]
    .filter((seconds) => seconds >= sso.session_min_seconds && seconds <= sso.session_max_seconds)
    .sort((a, b) => a - b)
  const failed = change.isError && !(change.error instanceof ApiError && change.error.code === "sso_review_required")

  return (
    <Section
      title="Single sign-on"
      action={identity.can_manage && <PortalButton team={team} intent="sso">{connected ? "Edit SSO" : "Set up SSO"}</PortalButton>}
    >
      <Row label="Identity provider">
        {sso.connections.length ? (
          <State ok={connected}>
            {sso.connections
              .map((connection) => providerName(connection.type) + (connection.state === "active" ? "" : ` (${connection.state})`))
              .join(", ")}
          </State>
        ) : (
          <span className="text-muted-foreground">Not set up</span>
        )}
      </Row>
      <Toggle
        label="Require SSO"
        description="Members sign in only through your identity provider, except those you exempt."
        checked={sso.required}
        disabled={!identity.can_manage || change.isPending || (!sso.required && !connected)}
        onChange={(required) => change.mutate({ required })}
      />
      <Toggle
        label="Join on first sign-in"
        description="People from your verified domains become members the first time they sign in with SSO."
        checked={sso.jit}
        disabled={!identity.can_manage || change.isPending}
        onChange={(jit) => change.mutate({ jit })}
      />
      <Row label="Session length" htmlFor="sso-session">
        <select
          id="sso-session"
          className="h-8 rounded-md border border-input bg-transparent px-2 text-sm"
          value={sso.session_seconds}
          disabled={!identity.can_manage || change.isPending}
          onChange={(event) => change.mutate({ session_seconds: Number(event.target.value) })}
        >
          {sessions.map((seconds) => (
            <option key={seconds} value={seconds}>
              {sessionLabel(seconds)}
            </option>
          ))}
        </select>
      </Row>
      {failed && <p className="py-2 text-sm text-destructive">{change.error.message}</p>}
      <Exemptions team={team} identity={identity} emails={emails} />

      <AlertDialog open={Boolean(review)} onOpenChange={(open) => !open && setReview(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Require SSO?</AlertDialogTitle>
            <AlertDialogDescription>
              {review?.members.length === 1 ? "This member loses" : `These ${review?.members.length} members lose`}{" "}
              access until they sign in with SSO:
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ul className="max-h-48 overflow-y-auto text-sm">
            {review?.members.map((member) => (
              <li key={member.account_id} className="py-0.5">
                {member.email}
              </li>
            ))}
          </ul>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => review && change.mutate({ required: true, confirm: review.digest })}>
              Require SSO
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Section>
  )
}

function Toggle({
  label,
  description,
  checked,
  disabled,
  onChange,
}: {
  label: string
  description: string
  checked: boolean
  disabled: boolean
  onChange: (value: boolean) => void
}) {
  return (
    <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
      <div className="flex flex-col gap-0.5">
        <span>{label}</span>
        <span className="text-xs text-muted-foreground">{description}</span>
      </div>
      <Switch aria-label={label} checked={checked} disabled={disabled} onCheckedChange={onChange} />
    </div>
  )
}

/** Up to `exempt_max` members may still sign in without SSO (break-glass access). */
function Exemptions({ team, identity, emails }: { team: Team; identity: TeamIdentity; emails: Map<string, string> }) {
  const save = useSaveIdentity(team)
  const exempt = identity.sso.exempt
  const change = useMutation({
    mutationFn: ({ account, on }: { account: string; on: boolean }) => setExempt(team.team_id, account, on),
    onSuccess: save,
  })
  const others = team.members.filter((member) => member.active && !exempt.includes(member.account_id))
  const name = (account: string) => emails.get(account) ?? team.members.find((m) => m.account_id === account)?.display_name ?? account
  return (
    <div className="flex flex-col gap-2 border-b border-divider py-3 text-sm">
      <div className="flex items-center justify-between gap-6">
        <div className="flex flex-col gap-0.5">
          <span>Exempt from SSO</span>
          <span className="text-xs text-muted-foreground">
            Up to {identity.sso.exempt_max} members who can still sign in without SSO, for when the identity provider is down.
          </span>
        </div>
        {identity.can_manage && exempt.length < identity.sso.exempt_max && others.length > 0 && (
          <select
            aria-label="Exempt a member"
            className="h-8 max-w-56 rounded-md border border-input bg-transparent px-2 text-sm"
            value=""
            disabled={change.isPending}
            onChange={(event) => event.target.value && change.mutate({ account: event.target.value, on: true })}
          >
            <option value="">Add a member…</option>
            {others.map((member) => (
              <option key={member.account_id} value={member.account_id}>
                {name(member.account_id)}
              </option>
            ))}
          </select>
        )}
      </div>
      {exempt.map((account) => (
        <div key={account} className="flex items-center justify-between gap-4 pl-1">
          <span>{name(account)}</span>
          {identity.can_manage && (
            <Button variant="ghost" size="sm" disabled={change.isPending} onClick={() => change.mutate({ account, on: false })}>
              Remove
            </Button>
          )}
        </div>
      ))}
      {change.isError && <span className="text-xs text-destructive">{change.error.message}</span>}
    </div>
  )
}

const groupRoles = ["admin", "billing", "auditor", "member"]

function Directory({ team, identity }: { team: Team; identity: TeamIdentity }) {
  const save = useSaveIdentity(team)
  const time = useTimeFormat()
  const directory = identity.directory
  const change = useMutation({
    mutationFn: ({ roles, mappedOnly }: { roles: Record<string, string>; mappedOnly: boolean }) =>
      updateDirectory(team.team_id, roles, mappedOnly),
    onSuccess: save,
  })
  const current = Object.fromEntries(directory.groups.map((group) => [group.id, group.role]))
  return (
    <Section
      title="Directory sync"
      info={{
        title: "Directory sync",
        body: "Your company directory adds and removes members, and its groups decide their roles.",
      }}
      action={
        identity.can_manage && (
          <PortalButton team={team} intent="dsync">
            {directory.directory ? "Edit directory" : "Connect a directory"}
          </PortalButton>
        )
      }
    >
      {!directory.directory ? (
        <p className="py-3 text-sm text-muted-foreground">No directory connected.</p>
      ) : (
        <>
          <Row label="Directory">
            <State ok={!directory.error && ready(directory.directory.state)}>
              {providerName(directory.directory.type)} · {directory.users}{" "}
              {directory.users === 1 ? "person" : "people"}
            </State>
          </Row>
          <Row label="Last sync">{directory.synced_at ? time.dateTime(directory.synced_at) : "Not yet"}</Row>
          {directory.error && <p className="border-b border-divider py-3 text-sm text-destructive">{directory.error}</p>}
          <Toggle
            label="Only people in mapped groups"
            description="Only members of a group below that gives a role join the team."
            checked={directory.mapped_only}
            disabled={!identity.can_manage || change.isPending}
            onChange={(mappedOnly) => change.mutate({ roles: current, mappedOnly })}
          />
          {directory.groups.length ? (
            directory.groups.map((group) => (
              <Row key={group.id} label={group.name} htmlFor={`group-${group.id}`}>
                <select
                  id={`group-${group.id}`}
                  className="h-8 rounded-md border border-input bg-transparent px-2 text-sm"
                  value={group.role}
                  disabled={!identity.can_manage || change.isPending}
                  onChange={(event) =>
                    change.mutate({ roles: { ...current, [group.id]: event.target.value }, mappedOnly: directory.mapped_only })
                  }
                >
                  <option value="">No role</option>
                  {groupRoles.map((role) => (
                    <option key={role} value={role}>
                      {roleLabels[role]}
                    </option>
                  ))}
                </select>
              </Row>
            ))
          ) : (
            <p className="py-3 text-sm text-muted-foreground">The directory has no groups yet.</p>
          )}
          {change.isError && <p className="py-2 text-sm text-destructive">{change.error.message}</p>}
        </>
      )}
      {directory.directory && (
        <span className="pt-2 text-xs text-muted-foreground">
          <Badge variant="outline" className="mr-1.5 font-normal">
            Managed by directory
          </Badge>
          members change role or leave only through the directory.
        </span>
      )}
    </Section>
  )
}
