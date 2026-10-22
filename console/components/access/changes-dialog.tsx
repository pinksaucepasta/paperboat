"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { actionLabels, PolicyError, type AccessChange, type Policy, type Validation } from "@/lib/policy"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Review, then save or restore. Lists who gains and loses access, then runs the change with the
 * server's `confirm` digest. A 412 (access changed since the review) shows the new list to confirm
 * again; a 409 (someone else saved) offers to reload.
 */
export function ChangesDialog({
  workspace,
  title,
  verb,
  review,
  run,
  onClose,
  onDone,
}: {
  workspace: string
  title: string
  verb: string
  /** The changes known before running (the editor validates the draft); restore learns them from a 412. */
  review?: () => Promise<Validation>
  run: (fields: { message?: string; confirm?: string }) => Promise<Policy>
  onClose: () => void
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  const [message, setMessage] = useState("")
  const [pending, setPending] = useState<{ changes: AccessChange[]; confirm: string } | null>(null)
  const reviewed = useQuery({
    queryKey: ["policy-review", workspace, title],
    queryFn: () => review!(),
    enabled: Boolean(review),
    gcTime: 0,
    retry: false,
  })
  const changes = pending?.changes ?? reviewed.data?.access_changes
  const confirm = pending?.confirm ?? (reviewed.data?.access_changes.length ? reviewed.data.confirm : undefined)
  const save = useMutation({
    mutationFn: () => run({ message: message.trim() || undefined, confirm }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["policy", workspace] })
      onDone()
    },
    onError: (error) => {
      if (error instanceof PolicyError && error.status === 412 && error.details.confirm)
        setPending({ changes: error.details.access_changes ?? [], confirm: error.details.confirm })
    },
  })
  const conflict = save.error instanceof PolicyError && save.error.status === 409
  const unconfirmed = save.error instanceof PolicyError && save.error.status === 412
  const invalid = reviewed.data && !reviewed.data.valid

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        {review && reviewed.isPending ? (
          <Skeleton className="h-20 w-full" />
        ) : reviewed.isError ? (
          <p className="text-sm text-destructive">{reviewed.error.message}</p>
        ) : invalid ? (
          <p className="text-sm text-destructive">
            The file has {reviewed.data.errors.length} {reviewed.data.errors.length === 1 ? "error" : "errors"}. Fix
            them in the editor first.
          </p>
        ) : changes ? (
          <ChangeList changes={changes} />
        ) : (
          <p className="text-sm text-muted-foreground">Paperboat checks who gains or loses access before it saves.</p>
        )}
        {unconfirmed && (
          <p className="text-sm text-muted-foreground">Access changed since you opened this. Review the list again.</p>
        )}
        <div className="border-t border-divider pt-4">
          <Field className="gap-1.5">
            <FieldLabel htmlFor="policy-message" className="flex justify-between">
              Message <span className="font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="policy-message"
              value={message}
              onChange={(event) => setMessage(event.target.value)}
              maxLength={200}
            />
          </Field>
        </div>
        {save.isError && !unconfirmed && <p className="text-sm text-destructive">{save.error.message}</p>}
        {save.isError && save.error instanceof PolicyError && save.error.details.errors?.length ? (
          <ul className="flex flex-col gap-1 text-sm">
            {save.error.details.errors.map((problem, index) => (
              <li key={index}>
                <span className="font-mono text-xs">{problem.path || "/"}</span> {problem.message}
              </li>
            ))}
          </ul>
        ) : null}
        <DialogFooter>
          {conflict ? (
            <Button
              size="lg"
              onClick={async () => {
                await queryClient.invalidateQueries({ queryKey: ["policy", workspace] })
                onClose()
              }}
            >
              {review ? "Load the latest version (drops your edits)" : "Load the latest version"}
            </Button>
          ) : (
            <Button
              size="lg"
              disabled={save.isPending || Boolean(review && !reviewed.data?.valid)}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "Saving…" : changes?.length ? `${verb} and change access` : verb}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ChangeList({ changes }: { changes: AccessChange[] }) {
  if (!changes.length) return <p className="text-sm text-muted-foreground">Nobody gains or loses access.</p>
  return (
    <ul className="flex max-h-72 flex-col overflow-y-auto text-sm" data-access-changes>
      {changes.map((change, index) => (
        <li key={index} className="flex items-baseline gap-2 border-b border-divider py-1.5">
          <span className={change.change === "gained" ? "w-12 text-success" : "w-12 text-destructive"}>
            {change.change === "gained" ? "Gains" : "Loses"}
          </span>
          <span className="min-w-0 flex-1">
            <span className="font-medium">{change.subject}</span>{" "}
            <span className="text-muted-foreground">{actionLabels[change.action] ?? change.action} on</span>{" "}
            <span className="font-medium">{change.target}</span>
            {change.ports && <span className="text-muted-foreground"> · ports {change.ports.replaceAll(",", ", ")}</span>}
          </span>
        </li>
      ))}
    </ul>
  )
}
