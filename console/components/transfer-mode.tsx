"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useApiQuery } from "@/lib/api"
import { preferencesQuery, setTransferMode, transferModes, transferModesLive, type TransferMode } from "@/lib/transfer-mode"
import { Section } from "@/components/section"
import { Skeleton } from "@/components/ui/skeleton"

/** Settings → File transfers: Automatic or Unlimited, for this account on every machine. */
export function TransferModeSection() {
  const queryClient = useQueryClient()
  const preferences = useApiQuery(preferencesQuery)
  const change = useMutation({
    mutationFn: setTransferMode,
    onSuccess: (next) => queryClient.setQueryData(preferencesQuery.queryKey, next),
  })
  if (!transferModesLive) return null
  const current = change.variables ?? preferences.data?.file_transfer_mode
  return (
    <Section title="File transfers">
      {preferences.isPending ? (
        <Skeleton className="h-16 w-full" />
      ) : (
        <div role="radiogroup" aria-label="File transfers" className="flex flex-col gap-2 py-3 text-sm">
          {(Object.keys(transferModes) as TransferMode[]).map((mode) => (
            <label key={mode} className="flex cursor-pointer items-start gap-3">
              <input
                type="radio"
                name="file-transfer-mode"
                className="mt-1"
                checked={current === mode}
                disabled={change.isPending}
                onChange={() => change.mutate(mode)}
              />
              <span className="flex flex-col">
                <span className="font-medium">{transferModes[mode].label}</span>
                <span className="text-muted-foreground">{transferModes[mode].detail}</span>
              </span>
            </label>
          ))}
          {change.isError && <p className="text-destructive">{change.error.message}</p>}
        </div>
      )}
    </Section>
  )
}
