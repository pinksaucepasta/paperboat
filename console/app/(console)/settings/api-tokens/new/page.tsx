import type { Metadata } from "next"
import { NewToken } from "@/components/access/new-token"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { sections } from "@/lib/sections"

export const metadata: Metadata = { title: "Create token" }

export default function Page() {
  return (
    <>
      <PageHeader>
        <CrumbLink href={sections.apiTokens.href}>{sections.apiTokens.title}</CrumbLink>
        <Crumb>Create token</Crumb>
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        <NewToken />
      </main>
    </>
  )
}
