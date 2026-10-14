import type { Metadata } from "next"
import { Suspense } from "react"
import { TunnelPage } from "@/components/tunnels/tunnel-page"

export const metadata: Metadata = { title: "Tunnel" }

export default function Page({ params }: PageProps<"/tunnels/[id]">) {
  return (
    <Suspense>
      <Tunnel params={params} />
    </Suspense>
  )
}

async function Tunnel({ params }: Pick<PageProps<"/tunnels/[id]">, "params">) {
  const { id } = await params
  return <TunnelPage id={id} />
}
