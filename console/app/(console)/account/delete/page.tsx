import type { Metadata } from "next"
import { DeleteAccount } from "@/components/delete-account"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"

export const metadata: Metadata = { title: "Delete account" }

export default function DeleteAccountPage() {
  return (
    <>
      <PageHeader>
        <CrumbLink href="/account">Account</CrumbLink>
        <Crumb>Delete account</Crumb>
      </PageHeader>
      <main className="mx-auto flex w-full max-w-3xl flex-1 flex-col gap-8 px-4 py-6 md:px-8 md:py-8">
        <DeleteAccount />
      </main>
    </>
  )
}
