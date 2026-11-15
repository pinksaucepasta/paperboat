import type { Metadata } from "next"
import { Suspense } from "react"
import { PreviewPage } from "@/components/tunnels/preview-page"

export const metadata: Metadata = { title: "Preview" }

export default function Page({ params }: PageProps<"/previews/[id]">) {
  return (
    <Suspense>
      <Preview params={params} />
    </Suspense>
  )
}

async function Preview({ params }: Pick<PageProps<"/previews/[id]">, "params">) {
  const { id } = await params
  return <PreviewPage id={id} />
}
