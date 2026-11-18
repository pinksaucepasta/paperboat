"use client"

import { InfoIcon, WarningIcon, WarningOctagonIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { Button } from "@/components/ui/button"
import { useApiQuery } from "@/lib/api"
import { announcementsQuery, dismissBanner, type AnnouncementBanner as Banner } from "@/lib/announcements"

const icons = {
  info: <InfoIcon weight="fill" className="size-4 shrink-0" />,
  warning: <WarningIcon weight="fill" className="size-4 shrink-0 text-amber-600 dark:text-amber-400" />,
  critical: <WarningOctagonIcon weight="fill" className="size-4 shrink-0 text-destructive" />,
}

/** The link of an announcement: a console path stays in the console, an https address opens in a new tab. */
export function AnnouncementLink({ url, children }: { url: string; children: React.ReactNode }) {
  const className = "font-medium underline underline-offset-4"
  // The server checks this too; a link it did not vouch for is never rendered (javascript:, //host).
  if (!/^(https:\/\/\S+|\/(?!\/)[^\s\\]*)$/.test(url)) return null
  if (url.startsWith("/")) return <Link href={url} className={className}>{children}</Link>
  return <a href={url} target="_blank" rel="noreferrer noopener" className={className}>{children}</a>
}

/** The ribbon itself, for the page and for the operator's preview. */
export function BannerRibbon({ banner, onDismiss }: { banner: Pick<Banner, "title" | "body" | "link_url" | "severity" | "dismissible">; onDismiss?: () => void }) {
  return (
    <div
      role={banner.severity === "critical" ? "alert" : "status"}
      data-announcement-banner={banner.severity}
      className="flex items-start gap-3 border-b border-divider bg-surface px-4 py-2.5 text-sm md:px-6"
    >
      <span className="flex h-[1lh] items-center">{icons[banner.severity]}</span>
      <p className="min-w-0 flex-1 break-words">
        <span className="font-medium">{banner.title}</span>
        {banner.body && <span className="text-muted-foreground"> {banner.body}</span>}
        {banner.link_url && (
          <span className="ml-3 whitespace-nowrap">
            <AnnouncementLink url={banner.link_url}>Learn more</AnnouncementLink>
          </span>
        )}
      </p>
      {banner.dismissible && onDismiss && (
        <Button variant="ghost" size="icon" className="-my-1 -mr-2" aria-label="Dismiss announcement" onClick={onDismiss}>
          <XIcon />
        </Button>
      )}
    </div>
  )
}

/**
 * The operator's ribbon above every page: at most one, the most specific audience first. Dismissing it is
 * remembered for the account, not the browser. With nothing to say, nothing renders.
 */
export function AnnouncementBanner() {
  const queryClient = useQueryClient()
  const banner = useApiQuery(announcementsQuery).data?.banner
  const dismiss = useMutation({
    mutationFn: dismissBanner,
    // Gone at once; if the save fails the next read brings it back.
    onMutate: (id) => {
      queryClient.setQueryData(announcementsQuery.queryKey, (old) => (old && old.banner?.id === id ? { ...old, banner: null } : old))
    },
    onError: () => queryClient.invalidateQueries({ queryKey: announcementsQuery.queryKey }),
  })
  if (!banner) return null
  return <BannerRibbon banner={banner} onDismiss={() => dismiss.mutate(banner.id)} />
}
