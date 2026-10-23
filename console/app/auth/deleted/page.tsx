import type { Metadata } from "next"
import { BrandScreen } from "@/components/brand-screen"

export const metadata: Metadata = { title: "Account deleted" }

/** Where deleting the account ends: the session is gone, so this page needs none. */
export default function AccountDeletedPage() {
  return (
    <BrandScreen>
      <div className="flex flex-col items-center gap-3 text-center">
        <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em]">
          Your account is deleted
        </h1>
        <p className="max-w-md text-base text-white/75">
          Your machines, environment variables, previews, tunnels and settings are gone. We sent a confirmation to your
          email.
        </p>
      </div>
    </BrandScreen>
  )
}
