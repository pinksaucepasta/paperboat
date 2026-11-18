"use client"

import { CheckIcon, CopyIcon } from "@phosphor-icons/react"
import { useEffect, useState } from "react"
import { cn } from "@/lib/utils"

/** The console's CommandBlock: one line that scrolls sideways (or wraps, for a code shown once); a click copies it. */
export function CommandBlock({ command, label = "Copy command", wrap, ...rest }: { command: string; label?: string; wrap?: boolean } & Record<`data-${string}`, string | boolean>) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(timer)
  }, [copied])
  const copy = () => void navigator.clipboard?.writeText(command).then(() => setCopied(true), () => {})
  return (
    <div onClick={copy} className="group/copy relative flex w-full cursor-pointer rounded-lg bg-muted text-left hover:bg-accent has-focus-visible:ring-2 has-focus-visible:ring-ring/30">
      <code className={cn("no-scrollbar min-w-0 flex-1 py-3 pr-12 pl-3.5 font-mono text-xs leading-relaxed", wrap ? "break-all whitespace-normal" : "overflow-x-auto whitespace-pre")} {...rest}>
        {command}
      </code>
      <span className="pointer-events-none absolute inset-y-0 right-0 flex items-start rounded-r-lg bg-linear-to-l from-muted from-60% to-transparent py-3 pr-3 pl-8 group-hover/copy:from-accent">
        <button
          type="button"
          aria-label={copied ? "Copied" : label}
          onClick={(event) => {
            event.stopPropagation()
            copy()
          }}
          className="pointer-events-auto rounded-sm outline-none"
        >
          {copied ? <CheckIcon weight="bold" className="size-4 text-success" /> : <CopyIcon className="size-4 text-muted-foreground group-hover/copy:text-foreground" />}
        </button>
      </span>
    </div>
  )
}
