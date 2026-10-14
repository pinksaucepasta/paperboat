import { Suspense } from "react"
import { MachineShell } from "@/components/machine-shell"
import { CrumbLink, PageHeader } from "@/components/page-header"

export default function MachineLayout({ params, children }: LayoutProps<"/machines/[machineId]">) {
  return (
    <Suspense fallback={<MachineFallback />}>
      <Machine params={params}>{children}</Machine>
    </Suspense>
  )
}

async function Machine({ params, children }: Pick<LayoutProps<"/machines/[machineId]">, "params" | "children">) {
  const { machineId } = await params
  return <MachineShell machineId={machineId}>{children}</MachineShell>
}

function MachineFallback() {
  return (
    <PageHeader>
      <CrumbLink href="/">Machines</CrumbLink>
    </PageHeader>
  )
}
