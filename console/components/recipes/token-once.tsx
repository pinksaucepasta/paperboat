"use client"

import { WarningIcon } from "@phosphor-icons/react"
import { clientConfigQuery, useApiQuery } from "@/lib/api"
import { CommandBlock } from "@/components/add-machine-dialog"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"

/** The install lines for a recipe's token; `base` is the server's installer address (client configuration). */
export const installLines = (base: string, token: string) => ({
  posix: `curl -fsSL ${base}/recipe | PB_RECIPE=${token} bash`,
  windows: `$env:PB_RECIPE='${token}'; irm ${base}/recipe.ps1 | iex`,
  actions: `- run: curl -fsSL ${base}/recipe | bash\n  env: { PB_RECIPE: "\${{ secrets.PB_RECIPE }}" }`,
})

/** A recipe's token, shown once with the lines that install a machine from it. */
export function TokenOnce({ token, onDone }: { token: string; onDone: () => void }) {
  const base = useApiQuery(clientConfigQuery).data?.installer_base_url.replace(/\/+$/, "")
  const lines = base ? installLines(base, token) : undefined
  return (
    <div className="flex flex-col gap-4">
      <SectionHeading title="Token" />
      <p className="flex items-center gap-2 text-sm">
        <WarningIcon className="size-4 shrink-0 text-muted-foreground" />
        Copy it now. You won’t see it again.
      </p>
      <div data-recipe-token>
        <CommandBlock command={token} label="Copy token" wrap />
      </div>
      {lines && (
        <div className="flex flex-col gap-3 border-t border-divider pt-4">
          <span className="text-sm text-muted-foreground">Install on Linux or macOS</span>
          <CommandBlock command={lines.posix} />
          <span className="text-sm text-muted-foreground">Install on Windows</span>
          <CommandBlock command={lines.windows} />
          <details className="text-sm text-muted-foreground">
            <summary className="cursor-pointer">GitHub Actions</summary>
            <div className="mt-2 flex flex-col gap-2">
              <span>Save the token as the secret PB_RECIPE, then add a step:</span>
              <CommandBlock command={lines.actions} label="Copy step" />
            </div>
          </details>
        </div>
      )}
      <div>
        <Button size="lg" onClick={onDone}>
          I saved it
        </Button>
      </div>
    </div>
  )
}
