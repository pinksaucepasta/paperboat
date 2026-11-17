"use client"

import { CheckIcon, CopyIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { Button } from "@/components/ui/button"

/** A support/request reference with a copy button, shown wherever something failed. */
export function CopyReference({ reference }: { reference: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
      Reference <code className="font-mono" data-reference>{reference}</code>
      <Button
        variant="ghost"
        size="icon-xs"
        aria-label="Copy reference"
        onClick={() => {
          navigator.clipboard?.writeText(reference).then(
            () => {
              setCopied(true)
              setTimeout(() => setCopied(false), 1500)
            },
            () => {},
          )
        }}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>
    </span>
  )
}
