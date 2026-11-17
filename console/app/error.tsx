"use client"

import { Button } from "@/components/ui/button"
import { ErrorReference } from "@/components/error-reference"

/** A page that crashed while rendering: what happened, Try again, and the reference to send to support. */
export default function RouteError({ error, retry }: { error: Error & { digest?: string }; retry: () => void }) {
  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-3 px-4 text-center">
      <h1 className="text-lg font-medium">Something went wrong</h1>
      <p className="max-w-md text-sm text-muted-foreground">
        This page couldn’t be shown. Try again; if it keeps happening, send us the reference below.
      </p>
      <Button variant="outline" onClick={() => retry()}>
        Try again
      </Button>
      <ErrorReference error={error} />
    </div>
  )
}
