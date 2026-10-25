"use client"

import Link from "next/link"
import { meQuery, useApiQuery } from "@/lib/api"
import { accountDeletionLive } from "@/lib/account-deletion"
import { setHideEmail, useHideEmail } from "@/lib/privacy"
import { Email } from "@/components/email"
import { TransferModeSection } from "@/components/transfer-mode"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"

export function Account() {
  const { data: me } = useApiQuery(meQuery)
  const hideEmail = useHideEmail()
  return (
    <>
      <Section title="Profile">
        <Row label="Name">{me ? me.display_name || "—" : <Skeleton className="h-4 w-32" />}</Row>
        <Row label="Email">{me ? <Email value={me.email} revealable /> : <Skeleton className="h-4 w-48" />}</Row>
      </Section>
      <Section title="Privacy">
        <Row label="Hide my email" htmlFor="hide-email">
          <Switch id="hide-email" checked={hideEmail} onCheckedChange={setHideEmail} />
        </Row>
      </Section>
      <TransferModeSection />
      {accountDeletionLive && (
        <Section title="Delete account">
          <Row label="Delete your account and everything in it">
            <Button variant="outline" nativeButton={false} render={<Link href="/account/delete" />}>
              Delete account…
            </Button>
          </Row>
        </Section>
      )}
    </>
  )
}
