"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { api, useApiQuery, useWorkspace, type Workspace } from "@/lib/api"
import { teamAction, teamQuery } from "@/lib/teams"
import { preferencesQuery } from "@/lib/transfer-mode"
import { Section, SettingRow } from "@/components/section"
import { Switch } from "@/components/ui/switch"

const label = "Let commands on machines list variable names"
const helper = (
  <>
    <code className="font-mono text-code">pb env names</code> shows names only, never values. Commands started with{" "}
    <code className="font-mono text-code">pb env run</code> still get the values.
  </>
)

/** Environment page: whether `pb env names` on the active workspace's machines may list ENV names. */
export function EnvNamesSwitch() {
  const workspace = useWorkspace()
  // The static shell has no session yet: the same row, on and disabled, until the workspace is known.
  if (!workspace) return <EnvNamesSetting checked disabled error={null} onChange={() => {}} />
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
    <Section title="Settings">
      <SettingRow
        label={label}
        htmlFor="env-names-listable"
        description={
          <>
            {helper}
            {note && ` ${note}`}
          </>
        }
      >
        <Switch
          id="env-names-listable"
          checked={checked}
          disabled={disabled}
          onCheckedChange={onChange}
          aria-label={label}
        />
      </SettingRow>
      {error && <p className="py-2 text-sm text-destructive">{error.message}</p>}
    </Section>
  )
}
