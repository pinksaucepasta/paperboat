"use client"

import { PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { useApiQuery } from "@/lib/api"
import { cursorPageSize } from "@/lib/cursor-pages"
import { grantPermissions, setResourceGrant, teamQuery, type GrantKind, type Team } from "@/lib/teams"
import { hostOf, previewsPageQuery, tunnelsPageQuery } from "@/lib/tunnels"
import { ListPager } from "@/components/list-pager"
import { ConfirmAction, SearchField } from "@/components/list-toolbar"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"

type Grant = Team["grants"][number]
const kindLabels: Record<string, string> = { env: "ENV", preview: "Preview", tunnel: "Tunnel" }
const select =
  "h-9 rounded-md border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"

/** What members may do with the team's ENV, previews and tunnels; owners and admins grant and revoke. */
export function TeamGrants({ team, emails }: { team: Team; emails: Map<string, string> }) {
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const [revoking, setRevoking] = useState<Grant | null>(null)
  const grants = team.grants.filter((grant) => grant.active && kindLabels[grant.resource_kind])
  const refresh = () => queryClient.invalidateQueries({ queryKey: teamQuery(team.team_id).queryKey })
  const words = (grant: Grant) =>
    grantPermissions[grant.resource_kind as GrantKind]?.find((entry) => entry.id === grant.permission)?.label ??
    grant.permission
  return (
    <Section
      title="Grants"
      action={
        <Button variant="outline" size="lg" onClick={() => setAdding(true)}>
          <PlusIcon data-icon="inline-start" />
          Add
        </Button>
      }
    >
      {grants.length === 0 ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">No grants yet.</p>
      ) : (
        grants.map((grant) => (
          <div
            key={`${grant.account_id}/${grant.resource_kind}/${grant.resource_id}/${grant.permission}`}
            className="flex min-h-12 items-center justify-between gap-4 border-b border-divider text-sm"
          >
            <div className="flex min-w-0 flex-col">
              <span className="truncate">{emails.get(grant.account_id) ?? grant.account_id}</span>
              <span className="truncate text-muted-foreground">
                {kindLabels[grant.resource_kind]} · {words(grant)}
                {grant.resource_kind !== "env" && <span className="font-mono"> · {grant.resource_id}</span>}
              </span>
            </div>
            <Button variant="outline" size="sm" onClick={() => setRevoking(grant)}>
              Revoke
            </Button>
          </div>
        ))
      )}
      {adding && <AddGrant team={team} emails={emails} onClose={() => setAdding(false)} onDone={refresh} />}
      {revoking && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setRevoking(null)}
          title="Revoke this grant?"
          description={`${emails.get(revoking.account_id) ?? "The member"} loses ${words(revoking).toLowerCase()} on this ${kindLabels[revoking.resource_kind]}.`}
          action="Revoke"
          pending="Revoking…"
          mutation={{
            mutationFn: () =>
              setResourceGrant(
                team,
                {
                  account: revoking.account_id,
                  kind: revoking.resource_kind as GrantKind,
                  resource: revoking.resource_id,
                  permission: revoking.permission,
                },
                false,
              ),
            onSuccess: refresh,
          }}
        />
      )}
    </Section>
  )
}

function AddGrant({
  team,
  emails,
  onClose,
  onDone,
}: {
  team: Team
  emails: Map<string, string>
  onClose: () => void
  onDone: () => void
}) {
  const [account, setAccount] = useState("")
  const [kind, setKind] = useState<GrantKind>("env")
  const [permission, setPermission] = useState("read")
  const [resource, setResource] = useState("")
  const people = team.members.filter((member) => member.active)
  const chosen = kind === "env" ? team.team_id : resource
  const save = useMutation({
    mutationFn: () => setResourceGrant(team, { account, kind, resource: chosen, permission }, true),
    onSuccess: () => {
      onDone()
      onClose()
    },
  })
  return (
    <Dialog open onOpenChange={(open) => !open && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Add a grant</DialogTitle>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (account && chosen) save.mutate()
          }}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="grant-member">Member</FieldLabel>
            <select id="grant-member" className={select} value={account} onChange={(event) => setAccount(event.target.value)}>
              <option value="">Choose a member</option>
              {people.map((member) => (
                <option key={member.account_id} value={member.account_id}>
                  {emails.get(member.account_id) ?? member.display_name ?? member.account_id}
                </option>
              ))}
            </select>
          </Field>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="grant-kind">On</FieldLabel>
            <select
              id="grant-kind"
              className={select}
              value={kind}
              onChange={(event) => {
                const next = event.target.value as GrantKind
                setKind(next)
                setPermission(grantPermissions[next][0].id)
                setResource("")
              }}
            >
              {(Object.keys(kindLabels) as GrantKind[]).map((value) => (
                <option key={value} value={value}>
                  {kindLabels[value]}
                </option>
              ))}
            </select>
          </Field>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="grant-permission">Allow</FieldLabel>
            <select
              id="grant-permission"
              className={select}
              value={permission}
              onChange={(event) => setPermission(event.target.value)}
            >
              {grantPermissions[kind].map((entry) => (
                <option key={entry.id} value={entry.id}>
                  {entry.label}
                </option>
              ))}
            </select>
          </Field>
          {kind !== "env" && <ResourcePicker team={team.team_id} kind={kind} value={resource} onChange={setResource} />}
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!account || !chosen || save.isPending}>
              {save.isPending ? "Saving…" : "Grant"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** The team's previews or tunnels, searched and paged by the server; only attached ones can be granted. */
function ResourcePicker({
  team,
  kind,
  value,
  onChange,
}: {
  team: string
  kind: "preview" | "tunnel"
  value: string
  onChange: (id: string) => void
}) {
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const [page, setPage] = useState(1)
  const previews = useApiQuery({
    ...previewsPageQuery({ workspace: team, ended: false, q, page }),
    enabled: kind === "preview",
  })
  const tunnels = useApiQuery({ ...tunnelsPageQuery({ workspace: team, q, page }), enabled: kind === "tunnel" })
  const data = kind === "preview" ? previews.data : tunnels.data
  const rows = (data?.items ?? []).map((item) => ({
    id: item.id,
    name: "name" in item ? item.name : hostOf(item.endpoint),
  }))
  return (
    <div className="flex flex-col gap-2">
      <SearchField
        value={search}
        onChange={(next) => {
          setSearch(next)
          setPage(1)
        }}
        placeholder={kind === "preview" ? "Search previews" : "Search tunnels"}
      />
      <ul className="flex max-h-48 flex-col overflow-y-auto">
        {rows.length === 0 && (
          <li className="py-2 text-center text-sm text-muted-foreground">
            {data ? "Nothing attached to this team." : "Loading…"}
          </li>
        )}
        {rows.map((row) => (
          <li key={row.id}>
            <button
              type="button"
              onClick={() => onChange(row.id)}
              className={`flex w-full items-center rounded-md px-2 py-2 text-left text-sm hover:bg-accent ${value === row.id ? "font-medium" : ""}`}
            >
              <span className="truncate">{row.name}</span>
              {value === row.id && <span className="ml-auto text-muted-foreground">Chosen</span>}
            </button>
          </li>
        ))}
      </ul>
      {data && <ListPager page={page} size={cursorPageSize} total={data.total} go={setPage} />}
    </div>
  )
}
