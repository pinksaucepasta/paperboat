"use client"

import { InfoIcon } from "@phosphor-icons/react"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"

type Heading = {
  title: string
  /** An ⓘ next to the title explaining the section, with its own short heading. */
  info?: { title: string; body: React.ReactNode }
  /** A control on the right of the title, such as "Add". */
  action?: React.ReactNode
}

/**
 * A section's title row (32 px, the title on its cap height, an optional ⓘ and action). Section uses
 * it; a section whose body is a table, a panel or cards uses it alone over that body.
 */
export function SectionHeading({ title, info, action, className }: Heading & { className?: string }) {
  return (
    <div className="flex min-h-8 items-center gap-1.5">
      <h2 className={cn("flex items-center gap-1.5 text-sm font-medium", className)}>
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
  )
}

/** A titled page section (Account, a tunnel's hostnames, Network…), its rows divided below the title. */
export function Section({ children, ...heading }: Heading & { children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <SectionHeading {...heading} />
      <div className="flex flex-col border-t border-divider">{children}</div>
    </section>
  )
}

/**
 * The end of an object's page (DESIGN.md, Destructive actions): a faint red panel saying what goes,
 * and a light chip with red text that opens the confirmation (`children`: the dialogs).
 */
export function DangerZone({
  title,
  description,
  action,
  onAction,
  children,
}: {
  title: string
  description: React.ReactNode
  action: string
  onAction: () => void
  children?: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-3">
      <SectionHeading title="Danger zone" className="text-destructive" />
      <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <p className="text-sm font-medium">{title}</p>
          <p className="text-sm text-muted-foreground">{description}</p>
        </div>
        <Button
          variant="secondary"
          size="lg"
          className="bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
          onClick={onAction}
        >
          {action}
        </Button>
      </div>
      {children}
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

/** A setting inside a Section: label, a muted line under it, and its control (a switch) on the right. */
export function SettingRow({
  label,
  description,
  htmlFor,
  children,
}: {
  label: string
  description?: React.ReactNode
  htmlFor?: string
  children: React.ReactNode
}) {
  return (
    <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
      <div className="flex min-w-0 flex-col gap-0.5">
        <label htmlFor={htmlFor}>{label}</label>
        {description && <p className="text-muted-foreground">{description}</p>}
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  )
}
