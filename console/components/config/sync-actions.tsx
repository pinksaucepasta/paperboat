"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import {
  acceptConfigConsent,
  approveConfigRevision,
  configWarningQuery,
  forceConfig,
  forceWords,
  removeConfigConsent,
  type ConfigEnvironment,
  type ForceAction,
} from "@/lib/config-sync"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"

/** What can be done to a machine's sync from here; conflicts themselves are resolved in Git, on the machine. */
export function SyncActions({ environment }: { environment: ConfigEnvironment }) {
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["config-sync"] })
  const [dialog, setDialog] = useState<"terms" | "remove" | ForceAction | null>(null)
  const fenced = environment.assignment_version !== undefined && environment.repository_name
  const approve = useMutation({ mutationFn: () => approveConfigRevision(environment), onSuccess: refresh })
  if (!fenced) return null
  const pending = environment.consent_state === "pending"
  const waiting = (environment.review?.length ?? 0) > 0 && Boolean(environment.remote_revision)
  const force = environment.remote_revision && !pending
  const mode = environment.mode
  return (
    <Section title="Manage">
      {pending && (
        <SettingRow label="Syncing is waiting for you" description="Read what syncing keeps in the repository, then accept.">
          <Button size="sm" onClick={() => setDialog("terms")}>
            Review and accept
          </Button>
        </SettingRow>
      )}
      {waiting && (
        <SettingRow label="Approve repository changes" description="The machine applies them once you approve.">
          <Button size="sm" disabled={approve.isPending} onClick={() => approve.mutate()}>
            {approve.isPending ? "Approving…" : "Approve"}
          </Button>
        </SettingRow>
      )}
      {approve.isError && <p className="border-b border-divider py-2 text-sm text-destructive">{approve.error.message}</p>}
      {force && mode !== "push_only" && (
        <SettingRow label="Pull from repository" description="Replace this machine's files with the repository's.">
          <Button variant="outline" size="sm" onClick={() => setDialog("force_pull")}>
            Pull…
          </Button>
        </SettingRow>
      )}
      {force && mode !== "pull_only" && (
        <SettingRow label="Push to repository" description="Replace the repository's files with this machine's.">
          <Button variant="outline" size="sm" onClick={() => setDialog("force_push")}>
            Push…
          </Button>
        </SettingRow>
      )}
      {!pending && (
        <SettingRow label="Stop syncing" description="Removes your consent. The machine stops syncing at once.">
          <Button variant="outline" size="sm" onClick={() => setDialog("remove")}>
            Remove consent…
          </Button>
        </SettingRow>
      )}
      {dialog === "terms" && <Terms environment={environment} onClose={() => setDialog(null)} onDone={refresh} />}
      {(dialog === "force_pull" || dialog === "force_push") && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setDialog(null)}
          title={dialog === "force_pull" ? `Pull over ${environment.alias}'s files?` : "Push over the repository's files?"}
          description={
            dialog === "force_pull"
              ? "The repository's version replaces the files here, conflicts included. Replaced versions stay in Git history."
              : "This machine's version replaces the repository's files, conflicts included. Replaced versions stay in Git history."
          }
          action={dialog === "force_pull" ? "Pull" : "Push"}
          pending="Queuing…"
          confirmName={forceWords[dialog]}
          mutation={{ mutationFn: () => forceConfig(environment, dialog), onSuccess: refresh }}
        />
      )}
      {dialog === "remove" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setDialog(null)}
          title={`Stop syncing ${environment.alias}?`}
          description="It stops syncing now. Accept again to start."
          action="Remove consent"
          pending="Removing…"
          mutation={{ mutationFn: () => removeConfigConsent(environment), onSuccess: refresh }}
        />
      )}
    </Section>
  )
}

/** The server's own wording of what syncing means, then Accept. */
function Terms({
  environment,
  onClose,
  onDone,
}: {
  environment: ConfigEnvironment
  onClose: () => void
  onDone: () => void
}) {
  const warning = useQuery(configWarningQuery(environment.machine_id))
  const accept = useMutation({
    mutationFn: () => acceptConfigConsent(environment, warning.data!.revision),
    onSuccess: () => {
      onDone()
      onClose()
    },
  })
  const facts = warning.data
    ? [
        warning.data.repository_visibility,
        warning.data.history_retention,
        warning.data.access_consequence,
        warning.data.offline_behavior,
        warning.data.disable_action,
      ]
    : []
  return (
    <Dialog open onOpenChange={(open) => !open && !accept.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Sync {environment.alias}’s config</DialogTitle>
        <div className="flex flex-col gap-2 text-sm text-muted-foreground">
          {warning.isError ? (
            <p className="text-destructive">{warning.error.message}</p>
          ) : !warning.data ? (
            Array.from({ length: 4 }, (_, index) => <Skeleton key={index} className="my-0.5 h-4 w-full" />)
          ) : (
            facts.map((fact) => <p key={fact}>{fact}</p>)
          )}
        </div>
        {accept.isError && <p className="text-sm text-destructive">{accept.error.message}</p>}
        <DialogFooter>
          <Button variant="outline" size="lg" onClick={onClose} disabled={accept.isPending}>
            Cancel
          </Button>
          <Button size="lg" disabled={!warning.data || accept.isPending} onClick={() => accept.mutate()}>
            {accept.isPending ? "Accepting…" : "Accept"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
