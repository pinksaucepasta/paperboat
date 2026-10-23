import Link from "next/link"
import { ApiError } from "@/lib/api"
import { Button } from "@/components/ui/button"

const limitCodes = new Set(["machine_limit_reached", "preview_limit_reached", "browser_terminal_limit_reached"])

export const isLimitReached = (error: unknown): error is ApiError =>
  error instanceof ApiError && limitCodes.has(error.code)

/** Where Upgrade goes: the server's billing_url when it sends one, else the console's Billing page. */
function upgradeHref(error: ApiError) {
  const url = (error.details as { billing_url?: unknown } | undefined)?.billing_url
  return typeof url === "string" && url ? url : "/billing"
}

/** A plan limit was hit: the server's sentence, then what the person can do about it. */
export function LimitReached({ error, removeMachines }: { error: ApiError; removeMachines?: boolean }) {
  return (
    <div className="flex flex-col gap-2" role="alert">
      <p className="text-sm text-destructive">{error.message}</p>
      <div className="flex gap-2">
        {removeMachines && (
          <Button variant="secondary" size="sm" nativeButton={false} render={<Link href="/" />}>
            Remove machines
          </Button>
        )}
        <Button size="sm" nativeButton={false} render={<a href={upgradeHref(error)} />}>
          Upgrade
        </Button>
      </div>
    </div>
  )
}
