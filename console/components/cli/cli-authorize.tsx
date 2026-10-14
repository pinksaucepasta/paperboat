"use client"

import { CheckCircleIcon, CircleNotchIcon, XCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useRouter, useSearchParams } from "next/navigation"
import { useState } from "react"
import { api, ApiError, type Data } from "@/lib/api"
import { BrandScreen, brandPrimary, brandSecondary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

type DeviceRequest = Data<"/v1/auth/device/requests/{user_code}">

const deviceTypes: Record<DeviceRequest["device_type"], string> = {
  desktop: "computer",
  server: "server",
  container: "container",
}

const requestPath = (code: string) => `auth/device/requests/${encodeURIComponent(code)}`

/**
 * The page `pb login` opens. Shows which CLI asks to sign in to which account, and lets you
 * approve or deny it. Without a code in the link, asks for the code the CLI printed.
 */
export function CliAuthorize() {
  const code = useSearchParams().get("code")?.trim().toUpperCase() ?? ""
  return <BrandScreen>{code ? <Request code={code} /> : <CodeForm />}</BrandScreen>
}

function CodeForm() {
  const router = useRouter()
  const [value, setValue] = useState("")
  return (
    <form
      className="flex flex-col gap-5"
      onSubmit={(event) => {
        event.preventDefault()
        if (value.trim()) router.replace(`/cli/authorize?code=${encodeURIComponent(value.trim().toUpperCase())}`)
      }}
    >
      <Heading title="Sign in the Paperboat CLI" text="Enter the code shown in your terminal by pb login." />
      <Field>
        <FieldLabel htmlFor="cli-code" className="text-white/85">
          Code
        </FieldLabel>
        <Input
          id="cli-code"
          autoFocus
          autoComplete="off"
          spellCheck={false}
          className="border-white/30 bg-white/10 font-mono text-white uppercase placeholder:text-white/50"
          value={value}
          onChange={(event) => setValue(event.target.value)}
        />
      </Field>
      <Button type="submit" size="lg" className={brandPrimary} disabled={!value.trim()}>
        Continue
      </Button>
    </form>
  )
}

function Request({ code }: { code: string }) {
  const client = useQueryClient()
  const key = ["cli-authorize", code]
  const request = useQuery({
    queryKey: key,
    queryFn: () => api<DeviceRequest>(requestPath(code)),
    retry: false,
  })
  const decide = useMutation({
    mutationFn: (decision: "approve" | "deny") =>
      api<DeviceRequest>(`${requestPath(code)}/${decision}`, { method: "POST" }),
    onSuccess: (data) => client.setQueryData(key, data),
  })

  if (request.isPending) return <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
  if (request.isError) return <Failure error={request.error} />

  const data = request.data
  if (data.state === "approved" || data.state === "consumed") {
    return (
      <Outcome icon={<CheckCircleIcon className="size-8 text-white" />} title="CLI signed in">
        Return to your terminal; pb is signed in as {data.account.email}. You can close this tab.
      </Outcome>
    )
  }
  if (data.state === "denied") {
    return (
      <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="Sign-in denied">
        The CLI was not signed in. Run pb login again if this was a mistake.
      </Outcome>
    )
  }
  if (data.state === "expired") return <Expired />

  return (
    <div className="flex flex-col gap-5">
      <Heading
        title="Sign in the Paperboat CLI?"
        text={
          <>
            <span className="font-medium text-white">{data.client_label}</span> on a {data.os}{" "}
            {deviceTypes[data.device_type]} asks to use your account{" "}
            <span className="font-medium text-white">{data.account.email}</span>.
          </>
        }
      />
      <Field>
        <FieldLabel className="text-white/85">Code</FieldLabel>
        <p className="font-mono text-2xl tracking-[0.2em]">{data.user_code}</p>
        <FieldDescription className="text-white/75">
          Approve only if this matches the code in your terminal.
        </FieldDescription>
      </Field>
      {decide.isError && <p className="text-sm font-medium text-white">{decide.error.message}</p>}
      <div className="flex gap-2">
        <Button
          size="lg"
          className={`flex-1 ${brandPrimary}`}
          disabled={decide.isPending}
          onClick={() => decide.mutate("approve")}
        >
          Approve
        </Button>
        <Button
          size="lg"
          variant="secondary"
          className={`flex-1 ${brandSecondary}`}
          disabled={decide.isPending}
          onClick={() => decide.mutate("deny")}
        >
          Deny
        </Button>
      </div>
    </div>
  )
}

function Failure({ error }: { error: Error }) {
  const status = error instanceof ApiError ? error.status : 0
  if (status === 410) return <Expired />
  if (status === 404) {
    return (
      <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="Code not found">
        Check the code in your terminal, or run pb login again for a new one.
      </Outcome>
    )
  }
  return (
    <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="Couldn’t load the request">
      {error.message} Reload the page to try again.
    </Outcome>
  )
}

function Expired() {
  return (
    <Outcome icon={<XCircleIcon className="size-8 text-white/75" />} title="This code expired">
      Run pb login again for a new code.
    </Outcome>
  )
}

function Heading({ title, text }: { title: string; text: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em] text-balance">
        {title}
      </h1>
      <p className="text-base text-white/75">{text}</p>
    </div>
  )
}

function Outcome({ icon, title, children }: { icon: React.ReactNode; title: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 text-center">
      {icon}
      <h1 className="font-heading text-2xl font-semibold tracking-[-0.02em]">{title}</h1>
      <p className="text-base text-white/75">{children}</p>
    </div>
  )
}
