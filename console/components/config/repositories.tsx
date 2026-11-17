"use client"

import { PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery, useWorkspace } from "@/lib/api"
import {
  adoptTeamDefault,
  clearTeamConfigDefault,
  configAdoptionQuery,
  configRepositoriesPageSize,
  configRepositoriesQuery,
  connectRepository,
  disconnectRepository,
  repositoryCandidatesQuery,
  teamConfigDefaultQuery,
  unadoptTeamDefault,
  type ConfigRepository,
} from "@/lib/config-sync"
import { githubStatusQuery } from "@/lib/github"
import { usePageParam } from "@/lib/use-page-param"
import { ListPager } from "@/components/list-pager"
import { ChooseTeamDefault } from "@/components/team/team-config-default"
import { LoadError } from "@/components/load-error"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { ErrorMessage } from "@/components/error-reference"

/** The Git repositories your machines can sync config with: connect one, disconnect one. */
export function ConfigRepositories() {
  const queryClient = useQueryClient()
  const pages = usePageParam("config-repositories")
  const query = useApiQuery(configRepositoriesQuery(pages.page))
  const [connecting, setConnecting] = useState(false)
  const [removing, setRemoving] = useState<ConfigRepository | null>(null)
  const items = query.data?.items ?? []
  return (
    <Section
      title="Repositories"
      action={
        <Button variant="outline" size="lg" onClick={() => setConnecting(true)}>
          <PlusIcon data-icon="inline-start" />
          Connect
        </Button>
      }
    >
      {query.isError ? (
        <LoadError what="repositories" error={query.error} retry={() => query.refetch()} />
      ) : !query.data ? (
        <div className="flex min-h-12 items-center border-b border-divider">
          <Skeleton className="h-4 w-48" />
        </div>
      ) : items.length === 0 ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">No repositories connected.</p>
      ) : (
        items.map((repository) => (
          <div key={repository.id} className="flex min-h-12 items-center justify-between gap-4 border-b border-divider text-sm">
            <div className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{repository.display_name || repository.external_ref}</span>
              {repository.provider === "github" ? (
                <span className="truncate text-xs text-muted-foreground">GitHub</span>
              ) : (
                <span className="truncate font-mono text-xs text-muted-foreground">{repository.external_ref}</span>
              )}
            </div>
            <div className="flex shrink-0 items-center gap-4">
              <span className="hidden text-muted-foreground sm:inline">{usedBy(repository.machine_count ?? 0)}</span>
              <Button variant="outline" size="sm" onClick={() => setRemoving(repository)}>
                Disconnect
              </Button>
            </div>
          </div>
        ))
      )}
      {query.data && (
        <ListPager page={pages.page} size={configRepositoriesPageSize} total={query.data.pagination.total} go={pages.go} />
      )}
      {connecting && <ConnectDialog onClose={() => setConnecting(false)} />}
      {removing && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setRemoving(null)}
          title={`Disconnect ${removing.display_name || removing.external_ref}?`}
          description="Machines using it stop syncing. The repository itself is not touched."
          action="Disconnect"
          pending="Disconnecting…"
          mutation={{
            mutationFn: () => disconnectRepository(removing.id),
            onSuccess: () => queryClient.invalidateQueries({ queryKey: ["config-sync"] }),
          }}
        />
      )}
    </Section>
  )
}

const usedBy = (count: number) =>
  count === 0 ? "Not used" : `Used by ${count} ${count === 1 ? "machine" : "machines"}`

function ConnectDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  // GitHub repositories can be picked once your GitHub account is linked; any Git URL always works.
  const linked = useApiQuery(githubStatusQuery).data?.connected === true
  const [chosen, setSource] = useState<"github" | "git">("github")
  const source = linked ? chosen : "git"
  const done = () => {
    void queryClient.invalidateQueries({ queryKey: ["config-sync"] })
    onClose()
  }
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Connect a repository</DialogTitle>
        {linked && (
          <ToggleGroup
            value={[source]}
            onValueChange={(value) => value[0] && setSource(value[0] as "github" | "git")}
            variant="outline"
            spacing={0}
            className="self-start"
          >
            <ToggleGroupItem value="github">GitHub</ToggleGroupItem>
            <ToggleGroupItem value="git">Git URL</ToggleGroupItem>
          </ToggleGroup>
        )}
        {source === "github" ? <GitHubPicker onDone={done} /> : <GitUrlForm onDone={done} onCancel={onClose} />}
      </DialogContent>
    </Dialog>
  )
}

/** Your private GitHub repositories, paged by the server's cursor; picking one connects it. */
function GitHubPicker({ onDone }: { onDone: () => void }) {
  const [cursors, setCursors] = useState([""])
  const page = cursors.length
  const query = useQuery(repositoryCandidatesQuery(cursors[page - 1]))
  const connect = useMutation({
    mutationFn: (candidate: { external_id: string; display_name: string; default_branch: string }) =>
      connectRepository({
        provider: "github",
        external_ref: candidate.external_id,
        display_name: candidate.display_name,
        branch: candidate.default_branch || "main",
      }),
    onSuccess: onDone,
  })
  const next = query.data?.next_cursor
  const items = query.data?.items ?? []
  return (
    <div className="flex flex-col">
      {query.isError ? (
        <p className="py-3 text-sm text-muted-foreground">
          <ErrorMessage error={query.error} />
        </p>
      ) : !query.data ? (
        <Skeleton className="my-3 h-4 w-48" />
      ) : items.length === 0 ? (
        <p className="py-3 text-sm text-muted-foreground">No private repositories found on GitHub.</p>
      ) : (
        items.map((candidate) => (
          <div
            key={candidate.external_id}
            className="flex min-h-12 items-center justify-between gap-3 border-b border-divider text-sm"
          >
            <span className="min-w-0 truncate">{candidate.display_name}</span>
            <Button variant="outline" size="sm" disabled={connect.isPending} onClick={() => connect.mutate(candidate)}>
              Connect
            </Button>
          </div>
        ))
      )}
      {connect.isError && (
        <p className="pt-2 text-sm text-destructive">
          <ErrorMessage error={connect.error} />
        </p>
      )}
      {(page > 1 || next) && (
        <div className="flex justify-end gap-2 pt-3">
          <Button variant="outline" size="sm" disabled={page === 1} onClick={() => setCursors(cursors.slice(0, -1))}>
            Previous
          </Button>
          <Button variant="outline" size="sm" disabled={!next} onClick={() => next && setCursors([...cursors, next])}>
            Next
          </Button>
        </div>
      )}
    </div>
  )
}

function GitUrlForm({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [url, setUrl] = useState("")
  const [name, setName] = useState("")
  const [branch, setBranch] = useState("main")
  const connect = useMutation({
    mutationFn: () =>
      connectRepository({
        provider: "git",
        external_ref: url.trim(),
        branch: branch.trim() || "main",
        ...(name.trim() && { display_name: name.trim() }),
      }),
    onSuccess: onDone,
  })
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (url.trim()) connect.mutate()
      }}
    >
      <Field className="gap-1.5">
        <FieldLabel htmlFor="repository-url">Repository URL</FieldLabel>
        <Input id="repository-url" autoFocus value={url} onChange={(event) => setUrl(event.target.value)} />
      </Field>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="repository-name">
          Name <span className="ml-auto font-normal text-muted-foreground">Optional</span>
        </FieldLabel>
        <Input id="repository-name" value={name} onChange={(event) => setName(event.target.value)} />
      </Field>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="repository-branch">Branch</FieldLabel>
        <Input id="repository-branch" value={branch} onChange={(event) => setBranch(event.target.value)} />
      </Field>
      {connect.isError && (
        <p className="text-sm text-destructive">
          <ErrorMessage error={connect.error} />
        </p>
      )}
      <DialogFooter>
        <Button type="button" variant="outline" size="lg" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" size="lg" disabled={!url.trim() || connect.isPending}>
          {connect.isPending ? "Connecting…" : "Connect"}
        </Button>
      </DialogFooter>
    </form>
  )
}

/**
 * The selected team's default config repository: members use it on all their machines or stop;
 * owners and admins choose or remove it. Nothing in a Personal workspace.
 */
export function TeamConfigDefaults() {
  const team = useWorkspace()
  const adoption = useApiQuery(configAdoptionQuery)
  if (!team || team.kind !== "team") return null
  return (
    <Section title="Team default">
      <TeamDefaultRow
        team={team}
        adoptedTeam={adoption.data?.team_id}
        adoptedVersion={adoption.data?.adopted_version}
      />
    </Section>
  )
}

function TeamDefaultRow({
  team,
  adoptedTeam,
  adoptedVersion,
}: {
  team: { id: string; name: string; role: string }
  adoptedTeam?: string
  adoptedVersion?: number
}) {
  const queryClient = useQueryClient()
  const query = useQuery(teamConfigDefaultQuery(team.id))
  const [asking, setAsking] = useState(false)
  const [clearing, setClearing] = useState(false)
  const [choosing, setChoosing] = useState(false)
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["config-sync"] })
  const manage = team.role === "owner" || team.role === "admin"
  if (query.isPending) return <Skeleton className="my-3 h-4 w-48" />
  const choose = choosing && (
    <ChooseTeamDefault
      team={team.id}
      version={query.data?.version ?? 0}
      onClose={() => setChoosing(false)}
      onDone={() => {
        setChoosing(false)
        void refresh()
      }}
    />
  )
  const chooseButton = manage && (
    <Button variant="outline" size="sm" onClick={() => setChoosing(true)}>
      {query.data ? "Change" : "Choose"}
    </Button>
  )
  // A team without a default answers "unavailable", the same as one you may not read.
  if (!query.data)
    return (
      <div className="flex min-h-12 items-center justify-between gap-4 border-b border-divider text-sm">
        <span className="text-muted-foreground">No default repository.</span>
        {chooseButton}
        {choose}
      </div>
    )
  const using = adoptedTeam === team.id
  const stale = using && adoptedVersion !== query.data.version
  return (
    <div className="flex min-h-14 items-center justify-between gap-4 border-b border-divider text-sm">
      <div className="flex min-w-0 flex-col">
        <span className="truncate font-medium">{query.data.display_name}</span>
        <span className="truncate text-muted-foreground">
          {using ? (stale ? "Newer default available" : "In use on your machines") : "Not in use on your machines"}
        </span>
      </div>
      <div className="flex shrink-0 gap-2">
        {using && !stale ? (
          <ConfirmUnadopt onDone={refresh} />
        ) : (
          <Button variant="outline" size="sm" onClick={() => setAsking(true)}>
            {stale ? "Update" : "Use"}
          </Button>
        )}
        {chooseButton}
        {manage && (
          <Button variant="outline" size="sm" onClick={() => setClearing(true)}>
            Remove default
          </Button>
        )}
      </div>
      {choose}
      {asking && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setAsking(false)}
          title={`Use ${team.name}'s config on all your machines?`}
          description="Every machine of yours syncs from this repository, one way, replacing its current config setup."
          action="Use default"
          pending="Applying…"
          mutation={{ mutationFn: () => adoptTeamDefault(team.id, query.data.version), onSuccess: refresh }}
        />
      )}
      {clearing && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setClearing(false)}
          title={`Remove ${team.name}'s default repository?`}
          description="Machines already using it keep their current config setup."
          action="Remove default"
          pending="Removing…"
          mutation={{ mutationFn: () => clearTeamConfigDefault(team.id, query.data.version), onSuccess: refresh }}
        />
      )}
    </div>
  )
}

function ConfirmUnadopt({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        Stop using
      </Button>
      {open && (
        <ConfirmAction
          open
          onOpenChange={setOpen}
          title="Stop using the team default?"
          description="Your machines keep their current config setup."
          action="Stop using"
          pending="Stopping…"
          mutation={{ mutationFn: () => unadoptTeamDefault(), onSuccess: onDone }}
        />
      )}
    </>
  )
}
