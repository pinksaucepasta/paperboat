import type { Metadata } from "next"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { EditRecipe } from "@/components/recipes/edit-recipe"
import { WithSession } from "@/components/with-session"
import { sections } from "@/lib/sections"

export const metadata: Metadata = { title: "Edit recipe" }

export default async function Page({ params }: { params: Promise<{ name: string }> }) {
  const name = decodeURIComponent((await params).name)
  return (
    <WithSession>
      <PageHeader>
        <CrumbLink href={sections.recipes.href}>{sections.recipes.title}</CrumbLink>
        <CrumbLink href={`${sections.recipes.href}/${encodeURIComponent(name)}`}>{name}</CrumbLink>
        <Crumb>Edit</Crumb>
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        <EditRecipe name={name} />
      </main>
    </WithSession>
  )
}
