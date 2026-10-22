import type { Metadata } from "next"
import { Suspense } from "react"
import { InspectorPage } from "@/components/tunnels/inspector-page"

export const metadata: Metadata = { title: "Inspect preview" }

export default function Page({ params }: PageProps<"/previews/[id]/inspect">) {
  return (
    <Suspense>
      <Inspect params={params} />
    </Suspense>
  )
}

async function Inspect({ params }: Pick<PageProps<"/previews/[id]/inspect">, "params">) {
  const { id } = await params
  return <InspectorPage id={id} />
}
