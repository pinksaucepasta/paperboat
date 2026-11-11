import type { Metadata } from "next"
import { Suspense } from "react"
import { RecordingPlayer } from "@/components/recordings/recording-player"

export const metadata: Metadata = { title: "Recording" }

export default function Page({ params }: PageProps<"/recordings/[id]">) {
  return (
    <Suspense>
      <Player params={params} />
    </Suspense>
  )
}

async function Player({ params }: Pick<PageProps<"/recordings/[id]">, "params">) {
  const { id } = await params
  return <RecordingPlayer id={id} />
}
