import type { Metadata } from "next"
import { Suspense } from "react"
import { prefetchMachinesPage } from "@/lib/server/prefetch"
import { MachineQueryProvider, type MachineUrl } from "@/components/machine-query"
import { MachineTable } from "@/components/machine-table"
import { Crumb, PageHeader } from "@/components/page-header"
import { ServerData } from "@/components/server-data"
import { UsageLimits } from "@/components/usage-limits"

export const metadata: Metadata = { title: "Machines" }

function MachinesPage({ initial }: { initial?: MachineUrl }) {
  return (
    <>
      <PageHeader>
        <Crumb>Machines</Crumb>
      </PageHeader>
      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-8 px-4 py-6 md:px-8 md:py-8">
        <MachineQueryProvider initial={initial}>
          <UsageLimits />
          <MachineTable />
        </MachineQueryProvider>
      </main>
    </>
  )
}

async function Prefetched({ searchParams }: Pick<PageProps<"/">, "searchParams">) {
  const [{ state, workspace }, params] = await Promise.all([prefetchMachinesPage(), searchParams])
  const q = typeof params.q === "string" ? params.q : ""
  return (
    <ServerData state={state} workspace={workspace}>
      <MachinesPage initial={{ q, page: Number(params.page) || 1 }} />
    </ServerData>
  )
}

export default function HomePage({ searchParams }: PageProps<"/">) {
  // The shell renders at once; the prefetched page streams into the same response.
  return (
    <Suspense fallback={<MachinesPage />}>
      <Prefetched searchParams={searchParams} />
    </Suspense>
  )
}
