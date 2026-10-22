"use client"

import { UsersThreeIcon } from "@phosphor-icons/react"
import { useRouter } from "next/navigation"
import { useApiQuery, workspacesQuery } from "@/lib/api"
import { billingQuery, teamTrialDays } from "@/lib/billing"
import { setWorkspace } from "@/lib/workspace"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

const reasons = {
  member: "Add people with a team",
  machine: "Share machines with a team",
} as const

/**
 * Free and Pro are for one person; working with other people is Teams (pricing decisions
 * 2026-10-22). "Add member" and "Share machine" in Personal open this instead of failing: what a
 * team adds, the trial when the server offers one, what stays free, and the teams you're already in.
 */
export function TeamsUpgradeDialog({
  reason,
  open,
  onOpenChange,
}: {
  reason: keyof typeof reasons
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const router = useRouter()
  const workspaces = useApiQuery(workspacesQuery).data
  const personal = workspaces?.find((workspace) => workspace.kind === "personal")
  const teams = workspaces?.filter((workspace) => workspace.kind === "team") ?? []
  const billing = useApiQuery({ ...billingQuery(personal), enabled: open && Boolean(personal) }).data
  const trial = teamTrialDays(billing)

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="gap-5 sm:max-w-md">
        <DialogHeader>
          <UsersThreeIcon className="size-7 text-primary" />
          <DialogTitle>{reasons[reason]}</DialogTitle>
          <DialogDescription>
            Your personal workspace is just for you. A team gives its members lasting access to each other’s
            machines, with owners and admins deciding who uses what.
          </DialogDescription>
        </DialogHeader>
        <ul className="flex list-disc flex-col gap-1.5 pl-5 text-sm text-muted-foreground">
          {trial ? (
            <>
              <li>Free for {trial} days. Your card is taken at the start and billing begins on day {trial + 1}.</li>
              <li>We remind you 3 days before, and cancelling during the trial costs nothing.</li>
            </>
          ) : (
            <li>Teams are billed per seat. You take one seat and can add more any time.</li>
          )}
          <li>Sharing one terminal session or one preview link stays free, without a team.</li>
        </ul>
        {teams.length > 0 && (
          <div className="flex flex-col gap-2 text-sm">
            <span className="text-muted-foreground">Or use a team you’re in:</span>
            <div className="flex flex-wrap gap-2">
              {teams.map((team) => (
                <Button
                  key={team.id}
                  variant="outline"
                  onClick={() => {
                    setWorkspace(team.id)
                    onOpenChange(false)
                    router.push("/team")
                  }}
                >
                  {team.name}
                </Button>
              ))}
            </div>
          </div>
        )}
        <DialogFooter>
          <Button variant="ghost" size="lg" onClick={() => onOpenChange(false)}>
            Not now
          </Button>
          <Button
            size="lg"
            onClick={() => {
              onOpenChange(false)
              router.push("/team?create=1")
            }}
          >
            {trial ? `Start ${trial}-day free trial` : "Create team"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
