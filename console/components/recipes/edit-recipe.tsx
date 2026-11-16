"use client"

import { useQuery } from "@tanstack/react-query"
import { useWorkspace } from "@/lib/api"
import { recipeQuery } from "@/lib/recipes"
import { LoadError } from "@/components/load-error"
import { RecipeEditor } from "@/components/recipes/recipe-editor"
import { Skeleton } from "@/components/ui/skeleton"

/** The editor for an existing recipe, once its file is read. */
export function EditRecipe({ name }: { name: string }) {
  const workspace = useWorkspace()
  const query = useQuery(recipeQuery(workspace?.id ?? "", name))
  if (query.isError) return <LoadError what={name} error={query.error} retry={() => query.refetch()} />
  if (!query.data) return <Skeleton className="h-96 w-full" />
  return <RecipeEditor recipe={query.data.recipe} />
}
