"use client"

import { useEffect, useState } from "react"
import { toast } from "sonner"
import { errorReference, errorText } from "@/lib/error-reference"
import { reportShownError } from "@/lib/sentry-report"
import { CopyIcons, useCopy } from "@/components/copy-button"

/** "Reference: support_…" with a Copy button: the id to send to support, shown under every error. */
export function ErrorReference({ error, reference }: { error?: unknown; reference?: string }) {
  // The id is minted at first render and kept, so every render of one error shows the same one.
  const [shown] = useState(() => reference ?? errorReference(error))
  const { copied, copy } = useCopy(shown)
  useEffect(() => {
    if (reference === undefined) reportShownError(error)
  }, [error, reference])
  return (
    <span className="mt-1 flex flex-wrap items-center gap-x-1.5 text-xs font-normal text-muted-foreground">
      Reference:
      <code className="font-mono break-all" data-error-reference>
        {shown}
      </code>
      <button
        type="button"
        aria-label={copied ? "Copied" : "Copy reference"}
        className="inline-flex size-5 items-center justify-center rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
        onClick={(event) => {
          event.preventDefault()
          event.stopPropagation()
          copy()
        }}
      >
        <CopyIcons copied={copied} className="size-3.5" />
      </button>
    </span>
  )
}

/** An error's sentence (or `message`, when the page words it better) with its reference below. */
export function ErrorMessage({ error, message }: { error: unknown; message?: string }) {
  return (
    <>
      {message ?? errorText(error)}
      <ErrorReference error={error} />
    </>
  )
}

/** A failure toast: `title`, and under it the error's sentence and reference. */
export function errorToast(title: string, error: unknown) {
  toast.error(title, { description: <ErrorMessage error={error} />, duration: 12_000 })
}

/** A failure toast whose title is the error's own sentence, with the reference below. */
export function errorToastOf(error: unknown) {
  toast.error(errorText(error), { description: <ErrorReference error={error} />, duration: 12_000 })
}
