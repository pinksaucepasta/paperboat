import { Suspense } from "react"
import { MachineShell, MachineSkeleton } from "@/components/machine-shell"
import { Skeleton } from "@/components/ui/skeleton"
import { CrumbLink, PageHeader } from "@/components/page-header"
import { WithSession } from "@/components/with-session"
import { queryKeys } from "@/lib/query-keys"

export default function MachineLayout({ params, children }: LayoutProps<"/machines/[machineId]">) {
  return (
    <Suspense fallback={<MachineFallback />}>
      <Machine params={params}>{children}</Machine>
    </Suspense>
  )
}

async function Machine({ params, children }: Pick<LayoutProps<"/machines/[machineId]">, "params" | "children">) {
  const { machineId } = await params
  // Read on the server, the machine's header and tabs are in the first HTML.
  const reads = () => [{ key: queryKeys.machine(machineId), path: `machines/${encodeURIComponent(machineId)}` }]
  return (
    <WithSession reads={reads}>
      <MachineShell machineId={machineId}>{children}</MachineShell>
    </WithSession>
  )
}

/** The prerendered shell: the machine page's header and tabs before the machine id is known. */
function MachineFallback() {
  return (
    <>
      <PageHeader>
        <CrumbLink href="/">Machines</CrumbLink>
        <Skeleton className="my-1.5 h-4 w-28" />
      </PageHeader>
      <MachineSkeleton />
    </>
  )
}
