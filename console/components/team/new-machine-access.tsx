import Link from "next/link"
import type { Workspace } from "@/lib/api"

/**
 * What a machine added to a team can be used for before anyone is granted anything: nothing. The
 * server creates no grant at enrollment; an owner or admin gives access in Machine access.
 */
export function NewMachineAccess({ team, current, tone }: { team: Workspace; current?: boolean; tone?: "brand" }) {
  const quiet = tone === "brand" ? "text-white/75" : "text-muted-foreground"
  return (
    <p className={`text-xs ${quiet}`} data-new-machine-access>
      <span className={tone === "brand" ? "font-medium text-white" : "font-medium text-foreground"}>
        Who can use it
      </span>{" "}
      Nobody yet. An owner or admin of {team.name} gives members access under{" "}
      {current ? (
        <Link href="/team" className="underline underline-offset-4 hover:text-foreground">
          Machine access
        </Link>
      ) : (
        "Machine access"
      )}
      .
    </p>
  )
}
