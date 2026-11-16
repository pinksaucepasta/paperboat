"use client"

import { useSearchParams } from "next/navigation"
import { templates } from "@/lib/recipes"
import { RecipeEditor } from "@/components/recipes/recipe-editor"

/** The editor, started empty or from one of the empty list's examples (`?from=ci`). */
export function NewRecipe() {
  const from = useSearchParams().get("from")
  const start = templates.find((template) => template.id === from)
  return <RecipeEditor key={start?.id} start={start} />
}
