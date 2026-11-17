"use client"

import { Button } from "@/components/ui/button"
import { ApiError } from "@/lib/api"
import { CopyReference } from "@/components/copy-reference"

/**
 * A page or list that could not load: what failed in plain words, the reference to quote to support, and Retry
 * (a refusal such as 403 will not change on retry, so it gets none).
 */
export function ErrorBlock({ what, error, retry }: { what: string; error: Error; retry?: () => void }) {
  const api = error instanceof ApiError ? error : undefined
  const refused = api?.status === 403
  return (
    <div role="alert" className="flex flex-col items-center gap-2 py-12 text-center text-sm" data-error-block>
      <p className="font-medium">{refused ? `This role cannot see ${what}.` : `Couldn’t load ${what}.`}</p>
      <p className="max-w-md text-muted-foreground">{error.message}</p>
      {api?.reference && <CopyReference reference={api.reference} />}
      {!refused && retry && (
        <Button variant="outline" size="sm" onClick={retry}>
          Retry
        </Button>
      )}
    </div>
  )
}
