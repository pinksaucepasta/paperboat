"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { api, useApiQuery, useWorkspace, type Workspace } from "@/lib/api"
import { teamAction, teamQuery } from "@/lib/teams"
import { preferencesQuery } from "@/lib/transfer-mode"
import { Row } from "@/components/section"
import { Switch } from "@/components/ui/switch"

const label = "Commands on machines can list ENV names"
const helper =
  "Lets `pb env names` on your machines print the names of your ENV variables (never values), so an AI agent can find the secret it needs. Commands run with `pb env run` still receive the values."

/** Environment page: whether `pb env names` on the active workspace's machines may list ENV names. */
export function EnvNamesSwitch() {
  const workspace = useWorkspace()
  if (!workspace) return null
  return workspace.kind === "team" ? <TeamEnvNames team={workspace} /> : <PersonalEnvNames />
}

function PersonalEnvNames() {
  const queryClient = useQueryClient()
  const preferences = useApiQuery(preferencesQuery)
  const change = useMutation({
    mutationFn: (on: boolean) =>
      api<unknown>("me/preferences", {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ env_names_listable: on }),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: preferencesQuery.queryKey }),
  })
  // While the preference loads, the same row shows, off and disabled, so nothing moves when it lands.
  return (
    <EnvNamesSetting
      checked={(change.isPending ? change.variables : preferences.data?.env_names_listable) ?? !preferences.isPending}
      disabled={preferences.isPending || change.isPending}
      error={change.error}
      onChange={(on) => change.mutate(on)}
    />
  )
}

function TeamEnvNames({ team }: { team: Workspace }) {
  const queryClient = useQueryClient()
  const summary = useApiQuery(teamQuery(team.id))
  const change = useMutation({
    mutationFn: (on: boolean) => teamAction(summary.data!, "env_names", { env_names_listable: on }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: teamQuery(team.id).queryKey }),
  })
  // Only owners and admins change team settings; the server enforces it too.
  const manage = team.role === "owner" || team.role === "admin"
  return (
    <EnvNamesSetting
      checked={(change.isPending ? change.variables : summary.data?.env_names_listable) ?? !summary.isPending}
      disabled={!manage || summary.isPending || change.isPending}
      error={change.error}
      note={manage ? undefined : "Only owners and admins can change this for the team."}
      onChange={(on) => change.mutate(on)}
    />
  )
}

function EnvNamesSetting({
  checked,
  disabled,
  error,
  note,
  onChange,
}: {
  checked: boolean
  disabled: boolean
  error: Error | null
  note?: string
  onChange: (on: boolean) => void
}) {
  return (
    <div className="flex flex-col">
      <Row label={label} htmlFor="env-names-listable">
        <Switch
          id="env-names-listable"
          checked={checked}
          disabled={disabled}
          onCheckedChange={onChange}
          aria-label={label}
        />
      </Row>
      <p className="py-2 text-xs text-muted-foreground">{note ? `${helper} ${note}` : helper}</p>
      {error && <p className="pb-2 text-sm text-destructive">{error.message}</p>}
    </div>
  )
}
