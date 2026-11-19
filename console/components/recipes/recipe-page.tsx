"use client"

import { useQuery } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { useWorkspace } from "@/lib/api"
import { failedSections, recipeQuery, type RecipeMachine } from "@/lib/recipes"
import { sections } from "@/lib/sections"
import { LoadError } from "@/components/load-error"
import { StatusDot } from "@/components/machine-status"
import { RevokeRecipe, RotateRecipe } from "@/components/recipes/recipe-actions"
import { DangerZone, Row, Section, SectionHeading } from "@/components/section"
import { ListSkeleton, SectionSkeleton } from "@/components/skeletons"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { useShellState } from "@/lib/use-shell-state"

const base = sections.recipes.href

/** One recipe: its file, how it stands, its machines; rotate and revoke with the same impact text as the CLI. */
export function RecipePage({ name }: { name: string }) {
  const workspace = useWorkspace()
  const router = useRouter()
  const [dialog, setDialog] = useShellState<"rotate" | "revoke">("recipes/recipe-page.tsx:dialog")
  const query = useQuery(recipeQuery(workspace?.id ?? "", name))
  if (query.isError) return <LoadError what={name} error={query.error} retry={() => query.refetch()} />
  if (!query.data || !workspace) {
    return (
      <div className="flex flex-col gap-8">
        <SectionSkeleton title="Recipe" rows={4} />
        <section className="flex flex-col gap-3">
          <SectionHeading title="Machines" />
          <ListSkeleton rows={2} detail="w-64" />
        </section>
      </div>
    )
  }
  const { recipe, machines } = query.data
  const limit = recipe.max_machines ? ` of ${recipe.max_machines}` : ""
  return (
    <div className="flex flex-col gap-8">
      <Section
        title="Recipe"
        action={
          <div className="flex gap-2">
            <Button variant="outline" size="lg" onClick={() => setDialog("rotate")}>
              Rotate token
            </Button>
            <Button size="lg" render={<Link href={`${base}/${encodeURIComponent(recipe.name)}/edit`} />}>
              Edit
            </Button>
          </div>
        }
      >
        <Row label="Revision">{recipe.revision}</Row>
        <Row label="Token">
          <span className="font-mono text-xs">{recipe.token}</span>
        </Row>
        <Row label="Expires">
          <Time iso={recipe.expires_at} relative />
        </Row>
        <Row label="Created by">{recipe.created_by.replace(/^user:/, "")}</Row>
      </Section>
      <section className="flex flex-col gap-3">
        <SectionHeading title="File" />
        <pre data-recipe-file className="overflow-x-auto surface rounded-lg p-4 font-mono text-xs leading-relaxed">
          {recipe.source}
        </pre>
      </section>
      <section className="flex flex-col gap-3">
        <SectionHeading title={`Machines · ${recipe.machines}${limit}`} />
        {machines.length === 0 ? (
          <p className="border-t border-divider py-6 text-sm text-muted-foreground">No machines yet.</p>
        ) : (
          <ul className="flex flex-col border-t border-divider">
            {machines.map((machine) => (
              <MachineLine key={machine.id} machine={machine} revision={recipe.revision} />
            ))}
          </ul>
        )}
      </section>
      <DangerZone
        title="Revoke this recipe"
        description="Its token stops working and its machines are removed."
        action="Revoke"
        onAction={() => setDialog("revoke")}
      />
      {dialog === "rotate" && (
        <RotateRecipe workspace={workspace.id} recipe={recipe} onClose={() => setDialog(undefined)} />
      )}
      {dialog === "revoke" && (
        <RevokeRecipe
          workspace={workspace.id}
          recipe={recipe}
          onClose={() => setDialog(undefined)}
          onDone={() => router.push(base)}
        />
      )}
    </div>
  )
}

function MachineLine({ machine, revision }: { machine: RecipeMachine; revision: number }) {
  const failed = failedSections(machine)
  return (
    <li
      className="flex min-h-14 flex-wrap items-center gap-3 border-b border-divider py-2 text-sm"
      data-machine={machine.name}
    >
      <span className="min-w-0 flex-1">
        <span className="flex items-center gap-2">
          <Link href={`/machines/${machine.id}`} className="font-medium hover:underline">
            {machine.name}
          </Link>
          <StatusDot online={machine.online} />
        </span>
        <span className="block truncate text-muted-foreground">
          {machine.applied_revision === 0
            ? "Not applied yet"
            : machine.applied_revision === revision
              ? `Applied revision ${revision}`
              : `Applied revision ${machine.applied_revision}, now ${revision}`}
          {failed.length > 0 && <span className="text-destructive"> · {failed.join("; ")}</span>}
        </span>
      </span>
      <span className="text-muted-foreground">
        {machine.remove_at ? (
          <>
            removed <Time iso={machine.remove_at} relative />
          </>
        ) : machine.online ? (
          "Online"
        ) : machine.last_seen_at ? (
          <>
            seen <Time iso={machine.last_seen_at} relative />
          </>
        ) : (
          "Offline"
        )}
      </span>
    </li>
  )
}
