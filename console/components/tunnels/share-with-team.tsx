"use client"

import { useMutation } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { useApiQuery, workspacesQuery } from "@/lib/api"
import { attachResource } from "@/lib/teams"
import { Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { ErrorMessage } from "@/components/error-reference"

/**
 * Attach your preview or tunnel to a team you run, or detach it. Attaching alone lets nobody use it;
 * owners and admins then grant members under Team → Grants. The resource doesn't say whether it is
 * attached, so both actions stay available.
 */
export function ShareWithTeam({ kind, id }: { kind: "preview" | "tunnel"; id: string }) {
  const teams = (useApiQuery(workspacesQuery).data ?? []).filter(
    (workspace) => workspace.kind === "team" && (workspace.role === "owner" || workspace.role === "admin"),
  )
  const [team, setTeam] = useState("")
  const chosen = team || teams[0]?.id
  const change = useMutation({
    mutationFn: (active: boolean) => attachResource({ team_id: chosen! }, kind, id, active),
    onSuccess: (_, active) => toast.success(active ? "Attached to the team" : "Detached from the team"),
  })
  if (teams.length === 0) return null
  return (
    <Section title="Team">
      <SettingRow label="Attach to a team" description="Members can use it once you grant them under Team → Grants.">
        <div className="flex items-center gap-2">
          {teams.length > 1 && (
            <select
              aria-label="Team"
              className="h-7 rounded-md border border-input bg-transparent px-2 text-xs outline-none"
              value={chosen}
              onChange={(event) => setTeam(event.target.value)}
            >
              {teams.map((entry) => (
                <option key={entry.id} value={entry.id}>
                  {entry.name}
                </option>
              ))}
            </select>
          )}
          <Button variant="outline" size="sm" disabled={change.isPending} onClick={() => change.mutate(true)}>
            Attach
          </Button>
          <Button variant="outline" size="sm" disabled={change.isPending} onClick={() => change.mutate(false)}>
            Detach
          </Button>
        </div>
      </SettingRow>
      {change.isError && (
        <p className="py-2 text-sm text-destructive">
          <ErrorMessage error={change.error} />
        </p>
      )}
    </Section>
  )
}
