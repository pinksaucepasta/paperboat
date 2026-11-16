import type { Metadata } from "next"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { RecipePage } from "@/components/recipes/recipe-page"
import { WithSession } from "@/components/with-session"
import { sections } from "@/lib/sections"

export const metadata: Metadata = { title: "Recipe" }

export default async function Page({ params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  return (
    <WithSession>
      <PageHeader>
        <CrumbLink href={sections.recipes.href}>{sections.recipes.title}</CrumbLink>
        <Crumb>{decodeURIComponent(name)}</Crumb>
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        <RecipePage name={decodeURIComponent(name)} />
      </main>
    </WithSession>
  )
}
