import type { Metadata } from "next"
import { Account } from "@/components/account"
import { Crumb, PageHeader } from "@/components/page-header"

export const metadata: Metadata = { title: "Account" }

export default function AccountPage() {
  return (
    <>
      <PageHeader>
        <Crumb>Account</Crumb>
      </PageHeader>
      <main className="mx-auto flex w-full max-w-3xl flex-1 flex-col gap-10 px-4 py-6 md:px-8 md:py-8">
        <Account />
      </main>
    </>
  )
}
