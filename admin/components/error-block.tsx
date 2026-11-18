"use client"

import { Button } from "@/components/ui/button"
import { ApiError } from "@/lib/api"
import { CopyReference } from "@/components/copy-reference"
import { integrationState } from "@/lib/integrations"

/**
 * The console's LoadError: one muted line with Retry and the reference. A refusal (403) or a service this server has not
 * configured (`*_not_configured`) will not change on retry, so it shows the server's sentence instead. A provider that is
 * down (Stripe, PostHog, Sentry) is named: it is theirs to fix, not a failure of this panel.
 */
export function ErrorBlock({ what, error, retry }: { what: string; error: Error; retry?: () => void }) {
  const api = error instanceof ApiError ? error : undefined
  const state = integrationState(api?.code ?? "")
  const refused = api?.status === 403 || state.kind === "not_configured"
  return (
    <div role="alert" className="flex flex-col items-center justify-center gap-1 py-12 text-sm text-muted-foreground" data-error-block>
      {refused ? (
        <span className="text-center">{error.message}</span>
      ) : (
        <span className="flex items-center gap-3">
          {state.kind === "down" ? `${state.provider} isn’t answering.` : `Couldn’t load ${what}.`}
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
