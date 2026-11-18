import { InfoIcon, WarningIcon, WarningOctagonIcon, XIcon } from "@phosphor-icons/react/dist/ssr"

type Severity = "info" | "warning" | "critical"

const icons = {
  info: <InfoIcon weight="fill" className="size-4 shrink-0" />,
  warning: <WarningIcon weight="fill" className="size-4 shrink-0 text-amber-500" />,
  critical: <WarningOctagonIcon weight="fill" className="size-4 shrink-0 text-destructive" />,
}

/** How a ribbon looks in the console (the same markup as console/components/announcement-banner.tsx). */
export function BannerPreview({ title, body, link, severity, dismissible }: { title: string; body: string; link: string; severity: Severity; dismissible: boolean }) {
  return (
    <div className="flex items-start gap-3 border-y border-divider bg-surface px-4 py-2.5 text-sm" data-preview="banner">
      <span className="flex h-[1lh] items-center">{icons[severity]}</span>
      <p className="min-w-0 flex-1 break-words">
        <span className="font-medium">{title || "Title"}</span>
        {body && <span className="text-muted-foreground"> {body}</span>}
        {link && <span className="ml-3 whitespace-nowrap font-medium underline underline-offset-4">Learn more</span>}
      </p>
      {dismissible && <XIcon role="img" className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-label="Users can dismiss it" />}
    </div>
  )
}

/** How a notice looks in the console's bell. */
export function NoticePreview({ title, body, link }: { title: string; body: string; link: string }) {
  return (
    <div className="flex flex-col gap-2 rounded-md border border-divider px-3 py-3 text-sm" data-preview="notice">
      <div className="flex min-w-0 items-baseline gap-2">
        <span className="size-2 shrink-0 self-center rounded-full bg-primary" aria-hidden />
        <span className="min-w-0 break-words font-medium">{title || "Title"}</span>
      </div>
      {body && <p className="break-words text-muted-foreground">{body}</p>}
      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        <span>just now</span>
        {link && <span className="font-medium underline underline-offset-4">Learn more</span>}
      </div>
    </div>
  )
}
