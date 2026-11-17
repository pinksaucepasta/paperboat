"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery } from "@/lib/api"
import {
  clearTeamConfigDefault,
  configRepositoriesPageSize,
  configRepositoriesQuery,
  setTeamConfigDefault,
  teamConfigDefaultQuery,
} from "@/lib/config-sync"
import { ListPager } from "@/components/list-pager"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage } from "@/components/error-reference"

/** The config repository the team's members can adopt on their machines; owners and admins choose it from their own. */
export function TeamConfigDefault({ team }: { team: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(teamConfigDefaultQuery(team))
  const [choosing, setChoosing] = useState(false)
  const [clearing, setClearing] = useState(false)
  // No default answers "unavailable"; it is shown as none.
  const current = query.data
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["config-sync"] })
  return (
    <Section title="Config default">
      <SettingRow
        label={query.isPending ? "Default repository" : (current?.display_name ?? "No default repository")}
        description={current ? `Branch ${current.branch}` : "Members can use it for config sync on their machines."}
      >
        {query.isPending ? (
          <Skeleton className="h-7 w-20" />
        ) : (
          <div className="flex gap-2">
            {current && (
              <Button variant="outline" size="sm" onClick={() => setClearing(true)}>
                Remove
              </Button>
            )}
            <Button variant="outline" size="sm" onClick={() => setChoosing(true)}>
              {current ? "Change" : "Choose"}
            </Button>
          </div>
        )}
      </SettingRow>
      {choosing && (
        <ChooseTeamDefault
          team={team}
          version={current?.version ?? 0}
          onClose={() => setChoosing(false)}
          onDone={() => {
            setChoosing(false)
            void refresh()
          }}
        />
      )}
      {clearing && current && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setClearing(false)}
          title="Remove the default repository?"
          description="Members who use it keep their current setup. New members have no default."
          action="Remove"
          pending="Removing…"
          mutation={{ mutationFn: () => clearTeamConfigDefault(team, current.version), onSuccess: refresh }}
        />
      )}
    </Section>
  )
}

/** Owners and admins pick the team's default from their own connected repositories. */
export function ChooseTeamDefault({
  team,
  version,
  onClose,
  onDone,
}: {
  team: string
  version: number
  onClose: () => void
  onDone: () => void
}) {
  const [page, setPage] = useState(1)
  const repositories = useApiQuery(configRepositoriesQuery(page, team))
  const set = useMutation({ mutationFn: (id: string) => setTeamConfigDefault(team, id, version), onSuccess: onDone })
  return (
    <Dialog open onOpenChange={(open) => !open && !set.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Choose from your repositories</DialogTitle>
        <div className="flex flex-col">
          {repositories.isError ? (
            <p className="py-3 text-sm text-muted-foreground">
              <ErrorMessage error={repositories.error} />
            </p>
          ) : !repositories.data ? (
            <Skeleton className="my-3 h-4 w-48" />
          ) : repositories.data.items?.length ? (
            repositories.data.items.map((repository) => (
              <div
                key={repository.id}
                className="flex min-h-12 items-center justify-between gap-3 border-b border-divider text-sm"
              >
                <span className="min-w-0 truncate">{repository.display_name || repository.external_ref}</span>
                {repository.team_default ? (
                  <span className="shrink-0 text-muted-foreground">Current default</span>
                ) : (
                  <Button variant="outline" size="sm" disabled={set.isPending} onClick={() => set.mutate(repository.id)}>
                    Use
                  </Button>
                )}
              </div>
            ))
          ) : (
            <p className="py-3 text-sm text-muted-foreground">Connect a repository under Config first.</p>
          )}
          {repositories.data && (
            <ListPager
              page={page}
              size={configRepositoriesPageSize}
              total={repositories.data.pagination.total}
              go={setPage}
            />
          )}
          {set.isError && (
            <p className="pt-2 text-sm text-destructive">
              <ErrorMessage error={set.error} />
            </p>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
