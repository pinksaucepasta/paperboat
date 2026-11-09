"use client"

import Link from "next/link"
import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { meQuery, useApiQuery, useMachines, type Workspace } from "@/lib/api"
import {
  actionLabels,
  envGrant,
  gridRule,
  parsePolicy,
  readOnlyNote,
  tagsByMachine,
  type Policy,
  type PolicyRule,
} from "@/lib/policy"
import { machineCapabilities, memberEmails, teamInvitationsQuery, teamQuery, type Team } from "@/lib/teams"
import { Section } from "@/components/section"
import { AccessForm } from "@/components/team/machine-access"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"

type Row = { id: string; alias: string; mine: boolean }

/**
 * Per-machine access as today's grants show it (team: "Manage access", the machine-grants API),
 * the tags and groups the file defines, and the rules only the file can express.
 */
export function AccessSimple({
  workspace,
  policy,
  onEditFile,
}: {
  workspace: Workspace
  policy: Policy
  onEditFile: () => void
}) {
  const file = parsePolicy(policy.policy)
  const tags = tagsByMachine(file)
  const rules = (file?.rules ?? [])
    .map((rule, index) => ({ rule, index }))
    .filter(({ rule }) => !gridRule(rule) && !envGrant(rule))
  const envGrants = (file?.rules ?? []).map(envGrant).filter((grant) => grant !== undefined)
  const groups = Object.entries(file?.groups ?? {})
  const note = readOnlyNote(policy)
  return (
    <div className="flex flex-col gap-10">
      {note && <p className="text-sm text-muted-foreground">{note}</p>}
      {workspace.kind === "team" ? (
        <TeamMachines teamId={workspace.id} policy={policy} tags={tags} />
      ) : (
        <PersonalMachines tags={tags} />
      )}
      {workspace.kind === "team" && (
        <Section title="Team settings">
          <SettingRow label="Private access">
            {file?.allow_private_access ? "On: rules may give private access" : "Off"}
          </SettingRow>
          <SettingRow label="Team ENV">
            {envGrants.length === 0
              ? "Owners only"
              : `Owners, and ${envGrants.length} ${envGrants.length === 1 ? "member" : "members"}`}
          </SettingRow>
          {envGrants.map((grant) => (
            <div
              key={grant.email}
              className="flex min-h-11 items-center justify-between gap-6 border-b border-divider pl-6 text-sm"
            >
              <span className="truncate">{grant.email}</span>
              <span className="text-muted-foreground">{grant.write ? "Read and change" : "Read"}</span>
            </div>
          ))}
        </Section>
      )}
      {groups.length > 0 && (
        <Section title="Groups">
          {groups.map(([group, users]) => (
            <div
              key={group}
              className="flex min-h-12 flex-wrap items-center gap-2 border-b border-divider py-2 text-sm"
            >
              <span className="w-40 font-medium">{group.replace(/^group:/, "")}</span>
              {(Array.isArray(users) ? users : []).map((user) => (
                <Badge key={String(user)} variant="outline" className="font-normal text-muted-foreground">
                  {String(user).replace(/^user:/, "")}
                </Badge>
              ))}
            </div>
          ))}
        </Section>
      )}
      {rules.length > 0 && (
        <Section
          title="Rules in the policy file"
          action={
            <Button variant="ghost" onClick={onEditFile}>
              Edit
            </Button>
          }
        >
          {rules.map(({ rule, index }) => (
            <RuleRow key={index} rule={rule} index={index} />
          ))}
        </Section>
      )}
    </div>
  )
}

function SettingRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 truncate text-right">{children}</span>
    </div>
  )
}

/** A policy subject or target in words: `role:admin` → Admins, `env:team` → Team ENV, `user:a@b` → a@b. */
function named(value: string) {
  const roles: Record<string, string> = { "role:owner": "Owners", "role:admin": "Admins", "role:member": "Members" }
  if (value === "*") return "Everyone"
  if (value === "env:team") return "Team ENV"
  return roles[value] ?? value.replace(/^(user|group|machine):/, "").replace(/^tag:/, "tag ")
}

function RuleRow({ rule, index }: { rule: PolicyRule; index: number }) {
  const deny = Boolean(rule.deny)
  const actions = (rule.deny ?? rule.allow ?? []).map((action) => actionLabels[action] ?? action)
  const list = (values: unknown) => (Array.isArray(values) ? values.map(String).join(", ") : "")
  const names = (values: unknown) => (Array.isArray(values) ? values.map((value) => named(String(value))).join(", ") : "")
  return (
    <div
      className="flex min-h-12 flex-wrap items-center gap-x-3 gap-y-1 border-b border-divider py-2 text-sm"
      data-rule={`/rules/${index}`}
    >
      <Badge variant={deny ? "destructive" : "outline"} className="font-normal">
        {deny ? "Deny" : "Allow"}
      </Badge>
      <span className="min-w-0 flex-1">
        <span className="font-medium">{names(rule.src)}</span>
        <span className="text-muted-foreground"> → </span>
        <span className="font-medium">{names(rule.dst)}</span>
        <span className="block text-muted-foreground">
          {actions.join(", ")}
          {rule.ports && ` · ports ${list(rule.ports)}`}
          {rule.note && ` · ${rule.note}`}
        </span>
      </span>
    </div>
  )
}

/** A machine's tags; each opens the machines list filtered by it. */
function TagChips({ tags }: { tags: string[] | undefined }) {
  return (tags ?? []).map((tag) => (
    <Badge
      key={tag}
      variant="outline"
      className="font-normal text-muted-foreground hover:text-foreground"
      render={<Link href={`/?q=${encodeURIComponent(`tag:${tag.replace(/^tag:/, "")}`)}`} />}
    >
      {tag.replace(/^tag:/, "")}
    </Badge>
  ))
}

/**
 * Personal: who can use these machines in one answer instead of one identical line per machine.
 * A machine bound to a team (`shared`, NOTES.md) is also governed by that team's access.
 */
function PersonalMachines({ tags }: { tags: Map<string, string[]> }) {
  const machines = useMachines()
  if (machines.isPending) return <OverviewSkeleton />
  if (machines.isError)
    return <p className="text-sm text-destructive">Loading machines failed: {machines.error.message}</p>
  const shared = machines.data.filter((machine) => machine.shared)
  const onlyYou = machines.data.length - shared.length
  const byTag = new Map<string, string[]>()
  for (const machine of machines.data)
    for (const tag of tags.get(machine.alias) ?? []) byTag.set(tag, [...(byTag.get(tag) ?? []), machine.alias])
  return (
    <>
      <Section title="Who can use these machines">
        {machines.data.length === 0 ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">
            No machines yet. Machines you add are yours alone until you share them with a team.
          </p>
        ) : (
          <>
            <SettingRow label="Only you">{plural(onlyYou, "machine")}</SettingRow>
            {shared.length > 0 && (
              <SettingRow label="You and a team">{shared.map((machine) => machine.alias).join(", ")}</SettingRow>
            )}
            <p className="pt-3 text-sm text-muted-foreground">
              To let other people use a machine, share it with a team from the machine’s page. API tokens act as
              you, limited to their scope.
            </p>
          </>
        )}
      </Section>
      {byTag.size > 0 && (
        <Section title="Tags">
          {[...byTag].map(([tag, aliases]) => (
            <div key={tag} className="flex min-h-12 items-center gap-4 border-b border-divider py-2 text-sm">
              <span className="w-40 shrink-0">
                <TagChips tags={[tag]} />
              </span>
              <span className="min-w-0 truncate text-muted-foreground">{aliases.join(", ")}</span>
            </div>
          ))}
        </Section>
      )}
    </>
  )
}

const plural = (count: number, noun: string) => `${count} ${noun}${count === 1 ? "" : "s"}`

/** Team: every machine with what the whole team and individual members may do, and Manage access. */
function TeamMachines({ teamId, policy, tags }: { teamId: string; policy: Policy; tags: Map<string, string[]> }) {
  const team = useApiQuery(teamQuery(teamId))
  const invitations = useApiQuery(teamInvitationsQuery(teamId))
  const me = useApiQuery(meQuery)
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<Row | null>(null)
  if (team.isPending || me.isPending) return <OverviewSkeleton />
  if (team.isError) return <p className="text-sm text-destructive">Loading the team failed: {team.error.message}</p>
  if (me.isError) return <p className="text-sm text-destructive">Loading your account failed: {me.error.message}</p>
  const emails = memberEmails(team.data, invitations.data)
  emails.set(me.data.id, me.data.email)
  const rows: Row[] = (team.data.machines ?? [])
    .filter((m) => m.active)
    .map((m) => ({ id: m.machine_id, alias: m.alias, mine: m.owner_account === me.data.id }))
  const canManage = (row: Row) => policy.can_edit === "all" || (policy.can_edit === "own_machines" && row.mine)
  if (rows.length === 0)
    return (
      <Section title="Machines">
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          No machines in the team yet. Share one from the{" "}
          <Link href="/team" className="text-foreground underline-offset-4 hover:underline">
            Team
          </Link>{" "}
          page; nobody can use it until you give access here.
        </p>
      </Section>
    )
  return (
    <section>
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-[24%]" />
          <col />
          <col className="hidden w-[20%] md:table-column" />
          <col className="w-36" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Machine</th>
            <th>Everyone in the team can use</th>
            <th className="hidden md:table-cell">Individual access</th>
            <th aria-label="Actions" />
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {rows.map((row) => {
            const grants = teamGrants(team.data, row.id)
            return (
              <tr key={row.id} className="[&>td]:h-14 [&>td]:px-3">
                <td>
                  <span className="flex min-w-0 items-center gap-2">
                    <span className="truncate font-medium">{row.alias}</span>
                    <TagChips tags={tags.get(row.alias)} />
                  </span>
                </td>
                <td className={grants.everyone.length ? "truncate" : "truncate text-muted-foreground"}>
                  {grants.everyone.length ? grants.everyone.join(", ") : "Nothing yet"}
                </td>
                <td className="hidden text-muted-foreground md:table-cell">{grants.members ? plural(grants.members, "member") : "—"}</td>
                <td className="text-right">
                  {canManage(row) && (
                    <Button variant="outline" disabled={team.data.suspended} onClick={() => setEditing(row)}>
                      Manage access
                    </Button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <Dialog open={Boolean(editing)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent className="gap-4 sm:max-w-md">
          {editing && (
            <AccessForm
              team={team.data}
              machineId={editing.id}
              alias={editing.alias}
              emails={emails}
              onDone={() => {
                setEditing(null)
                // Grants are part of the policy; the file and its version change with them.
                void queryClient.invalidateQueries({ queryKey: ["policy", teamId] })
              }}
            />
          )}
        </DialogContent>
      </Dialog>
    </section>
  )
}

function teamGrants(team: Team, machineId: string) {
  const grants = (team.machine_grants ?? []).filter((g) => g.machine_id === machineId && g.active)
  const everyone = grants.find((g) => g.audience === "all_members")
  return {
    everyone: machineCapabilities.filter((c) => everyone?.capabilities.includes(c.id)).map((c) => c.label),
    members: grants.filter((g) => g.audience === "selected_member").length,
  }
}

/** The Overview's shape while the policy, machines or team load: a section heading and its rows. */
export function OverviewSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex min-h-8 items-center">
        <Skeleton className="h-4 w-48" />
      </div>
      <div className="flex flex-col border-t border-divider">
        {Array.from({ length: 2 }, (_, index) => (
          <div key={index} className="flex min-h-12 items-center justify-between border-b border-divider">
            <Skeleton className="h-4 w-24" />
            <Skeleton className="h-4 w-20" />
          </div>
        ))}
      </div>
    </div>
  )
}
