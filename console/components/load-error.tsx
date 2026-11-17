"use client"

import { isFinalRefusal } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { ErrorReference } from "@/components/error-reference"

/**
 * A list or page that couldn't load: one muted line, Retry and the error's reference. A refusal (403, such as a team that
 * requires SSO, or an used-up bandwidth allowance) shows the server's sentence instead, since retrying won't help.
 */
export function LoadError({ what, error, retry }: { what: string; error: Error; retry: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center gap-1 py-12 text-sm text-muted-foreground">
      {isFinalRefusal(error) ? (
        <span className="text-center">{error.message}</span>
      ) : (
        <span className="flex items-center gap-3">
          Couldn’t load {what}.
          <Button variant="outline" size="sm" onClick={retry}>
            Retry
          </Button>
        </span>
      )}
      <ErrorReference error={error} />
    </div>
  )
}
