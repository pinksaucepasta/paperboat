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
        <Section title="Private access">
          <div className="flex min-h-12 items-center border-b border-divider py-2 text-sm">
            {file?.allow_private_access
              ? "Rules may give private access to the team's machines."
              : "Off: no rule gives private access. Owners and admins turn it on in the team's settings."}
          </div>
        </Section>
      )}
      {workspace.kind === "team" && (
        <Section title="Team ENV">
          {envGrants.length === 0 ? (
            <p className="py-3 text-sm text-muted-foreground">
              No member has their own access. Owners always may; other rules are listed below.
            </p>
          ) : (
            envGrants.map((grant) => (
              <div key={grant.email} className="flex min-h-12 items-center justify-between border-b border-divider py-2 text-sm">
                <span>{grant.email}</span>
                <span className="text-muted-foreground">{grant.write ? "Read and change" : "Read"}</span>
              </div>
            ))
          )}
        </Section>
      )}
      <Section title="Groups">
        {Object.entries(file?.groups ?? {}).length === 0 ? (
          <p className="py-3 text-sm text-muted-foreground">No groups. Add them in the policy file.</p>
        ) : (
          Object.entries(file?.groups ?? {}).map(([group, users]) => (
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
          ))
        )}
      </Section>
      <Section
        title="From the policy file"
        action={
          <Button variant="ghost" onClick={onEditFile}>
            Edit
          </Button>
        }
      >
        {rules.length === 0 ? (
          <p className="py-3 text-sm text-muted-foreground">Every rule is shown above.</p>
        ) : (
          rules.map(({ rule, index }) => <RuleRow key={index} rule={rule} index={index} />)
        )}
      </Section>
    </div>
  )
}

function RuleRow({ rule, index }: { rule: PolicyRule; index: number }) {
  const deny = Boolean(rule.deny)
  const actions = (rule.deny ?? rule.allow ?? []).map((action) => actionLabels[action] ?? action)
  const list = (values: unknown) => (Array.isArray(values) ? values.map(String).join(", ") : "")
  return (
    <div
      className="flex min-h-12 flex-wrap items-center gap-x-3 gap-y-1 border-b border-divider py-2 text-sm"
      data-rule={`/rules/${index}`}
    >
      <Badge variant={deny ? "destructive" : "outline"} className="font-normal">
        {deny ? "Deny" : "Allow"}
      </Badge>
      <span className="min-w-0 flex-1">
        <span className="font-medium">{list(rule.src)}</span>
        <span className="text-muted-foreground"> → </span>
        <span className="font-medium">{list(rule.dst)}</span>
        <span className="block text-muted-foreground">
          {actions.join(", ")}
          {rule.ports && ` · ports ${list(rule.ports)}`}
          {rule.note && ` · ${rule.note}`}
        </span>
      </span>
      <span className="font-mono text-xs text-muted-foreground">/rules/{index}</span>
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

function PersonalMachines({ tags }: { tags: Map<string, string[]> }) {
  const machines = useMachines()
  if (machines.isPending) return <Skeleton className="h-24 w-full" />
  if (machines.isError)
    return <p className="text-sm text-destructive">Loading machines failed: {machines.error.message}</p>
  return (
    <Section title="Machines">
      {machines.data.length === 0 && <p className="py-3 text-sm text-muted-foreground">No machines yet.</p>}
      {machines.data.map((machine) => (
        <div
          key={machine.id}
          className="flex min-h-12 flex-wrap items-center gap-2 border-b border-divider py-2 text-sm"
        >
          <span className="min-w-0 flex-1">
            <span className="font-medium">{machine.alias}</span>
            <span className="block text-muted-foreground">You have full access as its owner.</span>
          </span>
          <TagChips tags={tags.get(machine.alias)} />
        </div>
      ))}
    </Section>
  )
}

function TeamMachines({ teamId, policy, tags }: { teamId: string; policy: Policy; tags: Map<string, string[]> }) {
  const team = useApiQuery(teamQuery(teamId))
  const invitations = useApiQuery(teamInvitationsQuery(teamId))
  const me = useApiQuery(meQuery)
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<Row | null>(null)
  if (team.isPending || me.isPending) return <Skeleton className="h-24 w-full" />
  if (team.isError) return <p className="text-sm text-destructive">Loading the team failed: {team.error.message}</p>
  if (me.isError) return <p className="text-sm text-destructive">Loading your account failed: {me.error.message}</p>
  const emails = memberEmails(team.data, invitations.data)
  emails.set(me.data.id, me.data.email)
  const rows: Row[] = (team.data.machines ?? [])
    .filter((m) => m.active)
    .map((m) => ({ id: m.machine_id, alias: m.alias, mine: m.owner_account === me.data.id }))
  const canManage = (row: Row) => policy.can_edit === "all" || (policy.can_edit === "own_machines" && row.mine)
  return (
    <Section title="Machines">
      {rows.length === 0 && <p className="py-3 text-sm text-muted-foreground">No machines in the team yet.</p>}
      {rows.map((row) => (
        <div key={row.id} className="flex min-h-12 flex-wrap items-center gap-2 border-b border-divider py-2 text-sm">
          <span className="min-w-0 flex-1">
            <span className="font-medium">{row.alias}</span>
            <span className="block truncate text-muted-foreground">{grantSummary(team.data, row.id)}</span>
          </span>
          <TagChips tags={tags.get(row.alias)} />
          {canManage(row) && (
            <Button variant="outline" disabled={team.data.suspended} onClick={() => setEditing(row)}>
              Manage access
            </Button>
          )}
        </div>
      ))}
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
    </Section>
  )
}

function grantSummary(team: Team, machineId: string) {
  const grants = (team.machine_grants ?? []).filter((g) => g.machine_id === machineId && g.active)
  const everyone = grants.find((g) => g.audience === "all_members")
  const labels = machineCapabilities.filter((c) => everyone?.capabilities.includes(c.id)).map((c) => c.label)
  const members = grants.filter((g) => g.audience === "selected_member").length
  return (
    (labels.length ? `Team: ${labels.join(", ")}` : "No access for the team") +
    (members ? ` · ${members} ${members === 1 ? "member" : "members"} with their own access` : "")
  )
}
