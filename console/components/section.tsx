"use client"

import { InfoIcon } from "@phosphor-icons/react"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

/** A titled page section (Account, a tunnel's hostnames, Network…), its rows divided below the title. */
export function Section({
  title,
  info,
  action,
  children,
}: {
  title: string
  /** An ⓘ next to the title explaining the section, with its own short heading. */
  info?: { title: string; body: React.ReactNode }
  /** A control on the right of the title, such as "Add". */
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex min-h-8 items-center gap-1.5">
        <h2 className="flex items-center gap-1.5 text-sm font-medium">
          <span className="trim-cap">{title}</span>
          {info && (
            <Tooltip>
              <TooltipTrigger
                render={
                  <span
                    className="inline-flex text-muted-foreground hover:text-foreground"
                    aria-label={`About ${title.toLowerCase()}`}
                  />
                }
              >
                <InfoIcon className="size-3.5" />
              </TooltipTrigger>
              <TooltipContent className="flex max-w-72 flex-col items-start gap-1 py-2">
                <span className="font-medium">{info.title}</span>
                <span className="opacity-80">{info.body}</span>
              </TooltipContent>
            </Tooltip>
          )}
        </h2>
        {action && <div className="ml-auto">{action}</div>}
      </div>
      <div className="flex flex-col border-t border-divider">{children}</div>
    </section>
  )
}

export function Row({ label, htmlFor, children }: { label: string; htmlFor?: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-12 items-center justify-between gap-6 border-b border-divider py-2 text-sm">
      <label htmlFor={htmlFor} className="text-muted-foreground">
        {label}
      </label>
      <div className="min-w-0 truncate text-right">{children}</div>
    </div>
  )
}
