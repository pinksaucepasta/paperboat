"use client"

import { useTimeFormat } from "@/lib/use-time-format"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

/**
 * A timestamp in the viewer's time zone. Hovering adds what is not already on screen: the UTC
 * time, plus the exact local time when the text is relative ("2 hours ago").
 */
export function Time({ iso, relative = false }: { iso: string; relative?: boolean }) {
  const time = useTimeFormat()
  return (
    <Tooltip>
      <TooltipTrigger render={<time dateTime={iso} className="cursor-default" />}>
        {relative ? time.relative(iso) : time.dateTime(iso)}
      </TooltipTrigger>
      <TooltipContent side="top" className="flex-col items-start gap-0.5">
        {relative && <span>{time.exact(iso).local}</span>}
        <span className={relative ? "opacity-70" : undefined}>{time.exact(iso).utc}</span>
      </TooltipContent>
    </Tooltip>
  )
}
