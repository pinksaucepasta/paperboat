"use client"

import { Button } from "@/components/ui/button"
import { ApiError } from "@/lib/api"
import { CopyReference } from "@/components/copy-reference"

/**
 * The console's LoadError: one muted line with Retry and the reference. A refusal (403) will not change on retry, so it
 * shows the server's sentence instead.
 */
export function ErrorBlock({ what, error, retry }: { what: string; error: Error; retry?: () => void }) {
  const api = error instanceof ApiError ? error : undefined
  const refused = api?.status === 403
  return (
    <div role="alert" className="flex flex-col items-center justify-center gap-1 py-12 text-sm text-muted-foreground" data-error-block>
      {refused ? (
        <span className="text-center">{error.message}</span>
      ) : (
        <span className="flex items-center gap-3">
          Couldn’t load {what}.
          {retry && (
            <Button variant="outline" size="sm" onClick={retry}>
              Retry
            </Button>
          )}
        </span>
      )}
      {api?.reference && <CopyReference reference={api.reference} />}
    </div>
  )
}
