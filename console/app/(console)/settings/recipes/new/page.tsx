import type { Metadata } from "next"
import { Suspense } from "react"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { NewRecipe } from "@/components/recipes/new-recipe"
import { sections } from "@/lib/sections"

export const metadata: Metadata = { title: "New recipe" }

export default function Page() {
  return (
    <>
      <PageHeader>
        <CrumbLink href={sections.recipes.href}>{sections.recipes.title}</CrumbLink>
        <Crumb>New recipe</Crumb>
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        <Suspense>
          <NewRecipe />
        </Suspense>
      </main>
    </>
  )
}
