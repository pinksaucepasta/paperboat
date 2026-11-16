"use client"

import { ArrowsClockwiseIcon, PlusIcon, ProhibitIcon, ScrollIcon } from "@phosphor-icons/react"
import { useQuery } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { useWorkspace } from "@/lib/api"
import { recipesQuery, templates, type Recipe } from "@/lib/recipes"
import { sections } from "@/lib/sections"
import { LoadError } from "@/components/load-error"
import { RevokeRecipe, RotateRecipe } from "@/components/recipes/recipe-actions"
import { RowMenus } from "@/components/row-menus"
import { SectionHeading } from "@/components/section"
import { TableRowsSkeleton } from "@/components/skeletons"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { useNow } from "@/lib/use-now"

const base = sections.recipes.href

/** Settings → Recipes: one file turns a computer into a configured machine. */
export function Recipes() {
  const workspace = useWorkspace()
  const list = useQuery(recipesQuery(workspace?.id ?? ""))
  const create = (
    <Button size="lg" render={<Link href={`${base}/new`} />}>
      <PlusIcon data-icon="inline-start" />
      New recipe
    </Button>
  )
  if (list.isError) return <LoadError what="recipes" error={list.error} retry={() => list.refetch()} />
  if (list.data?.length === 0) return <NoRecipes action={create} />
  return (
    <section className="flex flex-col gap-3">
      <SectionHeading title="Recipes" action={create} />
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col />
          <col className="hidden w-[20%] md:table-column" />
          <col className="w-[18%]" />
          <col className="hidden w-[16%] lg:table-column" />
          <col className="hidden w-[16%] lg:table-column" />
          <col className="w-12" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Name</th>
            <th className="hidden md:table-cell">Tags</th>
            <th>Machines</th>
            <th className="hidden lg:table-cell">Expires</th>
            <th className="hidden lg:table-cell">Last used</th>
            <th className="w-12" aria-label="Actions" />
          </tr>
        </thead>
        <tbody className="divide-y divide-divider">
          {!list.data && (
            <TableRowsSkeleton
              rows={3}
              columns={[
                { bar: "w-28" },
                { className: "hidden md:table-cell", bar: "w-20" },
                { bar: "w-12" },
                { className: "hidden lg:table-cell", bar: "w-24" },
                { className: "hidden lg:table-cell", bar: "w-20" },
                { empty: true },
              ]}
            />
          )}
          {workspace && list.data?.map((recipe) => <RecipeRow key={recipe.id} workspace={workspace.id} recipe={recipe} />)}
        </tbody>
      </table>
    </section>
  )
}

function RecipeRow({ workspace, recipe }: { workspace: string; recipe: Recipe }) {
  const now = useNow(60_000)
  const [dialog, setDialog] = useState<"rotate" | "revoke">()
  const expired = now > 0 && Date.parse(recipe.expires_at) < now
  const tags = recipe.recipe.tags ?? []
  return (
    <RowMenus
      label={`${recipe.name} actions`}
      entries={[
        { kind: "item", label: "Rotate token", icon: ArrowsClockwiseIcon, onSelect: () => setDialog("rotate") },
        { kind: "item", label: "Revoke", icon: ProhibitIcon, destructive: true, onSelect: () => setDialog("revoke") },
      ]}
      dialogs={
        <>
          {dialog === "rotate" && <RotateRecipe workspace={workspace} recipe={recipe} onClose={() => setDialog(undefined)} />}
          {dialog === "revoke" && (
            <RevokeRecipe workspace={workspace} recipe={recipe} onClose={() => setDialog(undefined)} onDone={() => setDialog(undefined)} />
          )}
        </>
      }
      row={(actions) => (
        <tr
          data-recipe={recipe.name}
          className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3"
        >
          <td>
            <div className="flex h-[1lh] min-w-0 items-center gap-2">
              <Link
                href={`${base}/${encodeURIComponent(recipe.name)}`}
                className="min-w-0 font-medium after:absolute after:inset-0"
              >
                <span className="block truncate-x trim-cap">{recipe.name}</span>
              </Link>
              {expired && (
                <Badge variant="outline" className="trim-cap font-normal text-muted-foreground">
                  Expired
                </Badge>
              )}
            </div>
          </td>
          <td className="hidden truncate text-muted-foreground md:table-cell">{tags.length ? tags.join(", ") : "-"}</td>
          <td className="tabular-nums">
            {recipe.machines}
            {recipe.max_machines ? ` / ${recipe.max_machines}` : ""}
          </td>
          <td className="hidden whitespace-nowrap text-muted-foreground lg:table-cell">
            <Time iso={recipe.expires_at} relative />
          </td>
          <td className="hidden whitespace-nowrap text-muted-foreground lg:table-cell">
            {recipe.last_used_at ? <Time iso={recipe.last_used_at} relative /> : "Never"}
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
    />
  )
}

function NoRecipes({ action }: { action: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <Empty className="py-12">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <ScrollIcon />
          </EmptyMedia>
          <EmptyTitle>No recipes yet</EmptyTitle>
          <EmptyDescription>A recipe pairs and sets up machines with one command.</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>{action}</EmptyContent>
      </Empty>
      <section className="flex flex-col gap-3">
        <SectionHeading title="Start from" />
        <ul className="grid gap-3 sm:grid-cols-3">
          {templates.map((template) => (
            <li key={template.id}>
              <Link
                href={`${base}/new?from=${template.id}`}
                className="surface flex h-full flex-col gap-0.5 rounded-lg p-4 text-sm outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30"
              >
                <span className="font-medium">{template.title}</span>
                <span className="text-muted-foreground">{template.about}</span>
              </Link>
            </li>
          ))}
        </ul>
      </section>
    </div>
  )
}
