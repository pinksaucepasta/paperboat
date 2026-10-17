import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { BillingPage } from "@/components/billing/billing-page"

export const metadata: Metadata = { title: sections.billing.title }

export default function Page() {
  return (
    <SectionPage id="billing">
      <div className="mx-auto max-w-3xl">
        <BillingPage />
      </div>
    </SectionPage>
  )
}
