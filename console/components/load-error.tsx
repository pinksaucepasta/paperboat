"use client"

import { isFinalRefusal } from "@/lib/api"
import { Button } from "@/components/ui/button"

/**
 * A list or page that couldn't load: one muted line and Retry. A refusal (403, such as a team that
 * requires SSO, or an used-up bandwidth allowance) shows the server's sentence instead, since retrying won't help.
 */
export function LoadError({ what, error, retry }: { what: string; error: Error; retry: () => void }) {
  return (
    <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
      {isFinalRefusal(error) ? (
        error.message
      ) : (
        <>
          Couldn’t load {what}.
          <Button variant="outline" size="sm" onClick={retry}>
            Retry
          </Button>
        </>
      )}
    </div>
  )
}
