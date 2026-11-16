import type { Metadata } from "next"
import { Suspense } from "react"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { RecipePage } from "@/components/recipes/recipe-page"
import { WithSession } from "@/components/with-session"
import { sections } from "@/lib/sections"

export const metadata: Metadata = { title: "Recipe" }

export default function Page({ params }: PageProps<"/settings/recipes/[name]">) {
  return (
    <WithSession>
      <Suspense>
        <Recipe params={params} />
      </Suspense>
    </WithSession>
  )
}

async function Recipe({ params }: Pick<PageProps<"/settings/recipes/[name]">, "params">) {
  const name = decodeURIComponent((await params).name)
  return (
    <>
      <PageHeader>
        <CrumbLink href={sections.recipes.href}>{sections.recipes.title}</CrumbLink>
        <Crumb>{name}</Crumb>
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        <RecipePage name={name} />
      </main>
    </>
  )
}
