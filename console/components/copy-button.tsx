"use client"

import { CheckIcon, CopyIcon } from "@phosphor-icons/react"
import { useCallback, useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
import { toast } from "sonner"
import { copyText } from "@/lib/clipboard"

/** Copies text and reports `copied` for a moment, for any control that wants the feedback. */
export function useCopy(value: string) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(timer)
  }, [copied])
  const copy = useCallback(() => {
    void copyText(value).then((done) => {
      if (done) setCopied(true)
      else toast.error("Couldn’t copy", { description: "Select it and copy it yourself." })
    })
  }, [value])
  return { copied, copy }
}

export function CopyIcons({ copied, className }: { copied: boolean; className?: string }) {
  return copied ? (
    <CheckIcon weight="bold" className={cn("size-4 text-success", className)} />
  ) : (
    <CopyIcon className={cn("size-4", className)} />
  )
}

export function CopyButton({
  value,
  label = "Copy",
  className,
}: {
  value: string
  label?: string
  className?: string
}) {
  const { copied, copy } = useCopy(value)
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label={copied ? "Copied" : label}
      className={cn("text-muted-foreground hover:text-foreground", className)}
      onClick={(event) => {
        event.preventDefault()
        event.stopPropagation()
        copy()
      }}
    >
      <CopyIcons copied={copied} />
    </Button>
  )
}

/** A whole row that copies its value when clicked; the icon shows what will happen and that it did. */
export function CopyRow({
  value,
  children,
  className,
}: {
  value: string
  children?: React.ReactNode
  className?: string
}) {
  const { copied, copy } = useCopy(value)
  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? "Copied" : `Copy ${value}`}
      className={cn(
        "group/copy flex w-full items-center justify-between gap-4 rounded-md py-1.5 pr-2 pl-2.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30",
        className,
      )}
    >
      <span className="min-w-0 truncate">{children ?? value}</span>
      <CopyIcons copied={copied} className={cn(!copied && "text-muted-foreground group-hover/copy:text-foreground")} />
    </button>
  )
}

/** Inline text that copies itself when clicked; the icon appears on hover and confirms the copy. */
export function CopyText({ value, className }: { value: string; className?: string }) {
  const { copied, copy } = useCopy(value)
  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? "Copied" : `Copy ${value}`}
      className={cn(
        "group/copy relative z-10 inline-flex max-w-full items-center gap-1.5 rounded-sm text-left outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30",
        className,
      )}
    >
      <span className="truncate">{value}</span>
      <CopyIcons
        copied={copied}
        className={cn(
          "size-3.5 shrink-0",
          !copied && "opacity-0 group-hover/copy:opacity-100 group-focus-visible/copy:opacity-100",
        )}
      />
    </button>
  )
}
