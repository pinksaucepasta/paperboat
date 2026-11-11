"use client"

import { KeyIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState, useSyncExternalStore } from "react"
import { useApiQuery } from "@/lib/api"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { grantRecordingKey, recordingKeyQuery, recordingMessage, waitingViewers } from "@/lib/recordings"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { Button } from "@/components/ui/button"

/**
 * Viewers added after the key was made have no copy of it until someone holding one seals it for them, in their
 * browser. A holder sees this until nobody is waiting.
 */
export function RecordingAccess({ teamId }: { teamId: string }) {
  const queryClient = useQueryClient()
  const key = useApiQuery({ ...recordingKeyQuery(teamId), enabled: Boolean(teamId) }).data
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const [asked, setAsked] = useState(false)
  const grant = useMutation({
    mutationFn: () => grantRecordingKey(teamId, key!),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(teamId).queryKey }),
  })
  const waiting = key ? waitingViewers(key).length : 0
  if (!key?.key || !key.my_key || waiting === 0) return null
  return (
    <div className="flex flex-col gap-3" data-recording-access>
      <div className="flex flex-wrap items-center gap-3 surface rounded-lg p-4 text-sm">
        <KeyIcon className="size-4 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1">
          {waiting === 1 ? "One person can replay but has" : `${waiting} people can replay but have`} no copy of the
          recording key yet.
        </span>
        <Button
          size="lg"
          disabled={grant.isPending}
          onClick={() => (unlocked ? grant.mutate() : setAsked(true))}
        >
          {grant.isPending ? "Giving access…" : "Give access"}
        </Button>
        {grant.isError && <p className="w-full text-destructive">{recordingMessage(grant.error)}</p>}
      </div>
      {asked && !unlocked && <EnvUnlock title="Unlock your vault to give them a copy of the recording key." />}
    </div>
  )
}
