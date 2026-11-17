"use client"

import { RecordIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { createRecordingKey, personalWorkspace, recordingKeyQuery, recordingMessage, type RecordingKeyState, type RecordingViewer } from "@/lib/recordings"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { ErrorMessage } from "@/components/error-reference"

/**
 * The personal workspace's recording key: made in this browser, encrypted to the owner's own ENV keys (the same vault
 * that unlocks ENV). Free, Pro and Teams people all have it; recordings stay on their machine.
 */
export function PersonalRecordingSetup({ state }: { state: RecordingKeyState }) {
  const queryClient = useQueryClient()
  const sealable = state.viewers.filter((viewer): viewer is Required<RecordingViewer> => Boolean(viewer.sharing_public_key))
  const create = useMutation({
    mutationFn: () => createRecordingKey(personalWorkspace, sealable),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(personalWorkspace).queryKey }),
  })
  return (
    <Empty className="py-16" data-recording-setup>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <RecordIcon />
        </EmptyMedia>
        <EmptyTitle>Record your terminals</EmptyTitle>
        <EmptyDescription>
          {sealable.length === 0
            ? "Recordings are encrypted with your ENV keys. Set up ENV first, then come back here."
            : "Recordings stay on the machine, encrypted with your ENV keys. Only you can replay them."}
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        {sealable.length === 0 ? (
          <Button size="lg" nativeButton={false} render={<Link href="/environment" />}>
            Set up ENV
          </Button>
        ) : (
          <Button size="lg" disabled={create.isPending} onClick={() => create.mutate()}>
            {create.isPending ? "Turning on…" : "Turn on recording"}
          </Button>
        )}
        {create.isError && <p className="text-sm text-destructive">{<ErrorMessage error={create.error} message={recordingMessage(create.error)} />}</p>}
      </EmptyContent>
    </Empty>
  )
}
