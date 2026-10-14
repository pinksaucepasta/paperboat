"use client"

import { CheckCircleIcon, CircleNotchIcon, XCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useRouter, useSearchParams } from "next/navigation"
import { useState } from "react"
import { api, ApiError, type Data } from "@/lib/api"
import { Logo } from "@/components/logo"
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
  return (
    <main className="flex min-h-svh flex-col items-center justify-center gap-8 px-6">
      <Logo className="h-6" />
      <div className="flex w-full max-w-sm flex-col gap-5">{code ? <Request code={code} /> : <CodeForm />}</div>
    </main>
  )
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
        <FieldLabel htmlFor="cli-code">Code</FieldLabel>
        <Input
          id="cli-code"
          autoFocus
          autoComplete="off"
          spellCheck={false}
          className="font-mono uppercase"
          value={value}
          onChange={(event) => setValue(event.target.value)}
        />
      </Field>
      <Button type="submit" size="lg" disabled={!value.trim()}>
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

  if (request.isPending) return <CircleNotchIcon className="mx-auto size-5 animate-spin text-muted-foreground" />
  if (request.isError) return <Failure error={request.error} />

  const data = request.data
  if (data.state === "approved" || data.state === "consumed") {
    return (
      <Outcome icon={<CheckCircleIcon className="size-6 text-primary" />} title="CLI signed in">
        Return to your terminal; pb is signed in as {data.account.email}. You can close this tab.
      </Outcome>
    )
  }
  if (data.state === "denied") {
    return (
      <Outcome icon={<XCircleIcon className="size-6 text-destructive" />} title="Sign-in denied">
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
            <span className="font-medium text-foreground">{data.client_label}</span> on a {data.os}{" "}
            {deviceTypes[data.device_type]} asks to use your account{" "}
            <span className="font-medium text-foreground">{data.account.email}</span>.
          </>
        }
      />
      <Field>
        <FieldLabel>Code</FieldLabel>
        <p className="font-mono text-lg tracking-widest">{data.user_code}</p>
        <FieldDescription>Approve only if this matches the code in your terminal.</FieldDescription>
      </Field>
      {decide.isError && <p className="text-sm text-destructive">{decide.error.message}</p>}
      <div className="flex gap-2">
        <Button size="lg" className="flex-1" disabled={decide.isPending} onClick={() => decide.mutate("approve")}>
          Approve
        </Button>
        <Button
          size="lg"
          variant="outline"
          className="flex-1"
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
      <Outcome icon={<XCircleIcon className="size-6 text-destructive" />} title="Code not found">
        Check the code in your terminal, or run pb login again for a new one.
      </Outcome>
    )
  }
  return (
    <Outcome icon={<XCircleIcon className="size-6 text-destructive" />} title="Couldn’t load the request">
      {error.message} Reload the page to try again.
    </Outcome>
  )
}

function Expired() {
  return (
    <Outcome icon={<XCircleIcon className="size-6 text-muted-foreground" />} title="This code expired">
      Run pb login again for a new code.
    </Outcome>
  )
}

function Heading({ title, text }: { title: string; text: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <h1 className="font-heading text-xl font-semibold">{title}</h1>
      <p className="text-sm text-muted-foreground">{text}</p>
    </div>
  )
}

function Outcome({ icon, title, children }: { icon: React.ReactNode; title: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 text-center">
      {icon}
      <h1 className="font-heading text-lg font-semibold">{title}</h1>
      <p className="text-sm text-muted-foreground">{children}</p>
    </div>
  )
}
