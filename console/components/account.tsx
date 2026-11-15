"use client"

import { useRouter } from "next/navigation"
import { meQuery, useApiQuery } from "@/lib/api"
import { accountDeletionLive } from "@/lib/account-deletion"
import { setAnalyticsOff, setHideEmail, useAnalyticsOff, useHideEmail } from "@/lib/privacy"
import { Email } from "@/components/email"
import { TransferModeSection } from "@/components/transfer-mode"
import { DangerZone, Section, SettingRow } from "@/components/section"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"

export function Account() {
  const router = useRouter()
  const { data: me } = useApiQuery(meQuery)
  const hideEmail = useHideEmail()
  const analyticsOff = useAnalyticsOff()
  return (
    <>
      <Section title="Profile">
        <SettingRow label="Name">{me ? me.display_name || "Not set" : <Skeleton className="h-4 w-32" />}</SettingRow>
        <SettingRow label="Email">{me ? <Email value={me.email} revealable /> : <Skeleton className="h-4 w-48" />}</SettingRow>
      </Section>
      <Section title="Privacy">
        <SettingRow
          label="Hide my email"
          htmlFor="hide-email"
          description="Blurs your email in this browser, for screen sharing and screenshots."
        >
          <Switch id="hide-email" checked={hideEmail} onCheckedChange={setHideEmail} />
        </SettingRow>
        <SettingRow
          label="Share usage statistics"
          htmlFor="usage-statistics"
          description="Counts steps like adding a machine or starting a preview, tied to a random account id. Never names, emails, commands or content. This browser only."
        >
          <Switch id="usage-statistics" checked={!analyticsOff} onCheckedChange={(on) => setAnalyticsOff(!on)} />
        </SettingRow>
      </Section>
      <TransferModeSection />
      {accountDeletionLive && (
        <DangerZone
          title="Delete account"
          description="The next page lists exactly what’s removed."
          action="Delete account…"
          onAction={() => router.push("/account/delete")}
        />
      )}
    </>
  )
}
