"use client"

import { CheckCircleIcon, CircleNotchIcon, XCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useRouter, useSearchParams } from "next/navigation"
import { useState } from "react"
import { api, ApiError, useApiQuery, workspacesQuery, type Data, type Workspace } from "@/lib/api"
import { BrandScreen, brandPrimary, brandSecondary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { WorkspacePicker } from "@/components/workspace-picker"

type DeviceRequest = Data<"/v1/auth/device/requests/{user_code}">

const requestPath = (code: string) => `auth/device/requests/${encodeURIComponent(code)}`

/**
 * The page `pb login` and the installer open. A computer that is not on your account yet asks to
 * join it: you choose the workspace and approve. A machine that is already added only signs in
 * again. Without a code in the link, asks for the code the terminal printed.
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
      <Heading title="Add this computer" text="Enter the code shown in your terminal." />
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
  const workspaces = useApiQuery(workspacesQuery)
  const decide = useMutation({
    mutationFn: ({ decision, workspaceId }: { decision: "approve" | "deny"; workspaceId?: string }) =>
      api<DeviceRequest>(`${requestPath(code)}/${decision}`, {
        method: "POST",
        ...(workspaceId && {
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ workspace_id: workspaceId }),
        }),
      }),
    onSuccess: (data) => client.setQueryData(key, data),
  })

  if (request.isPending || workspaces.isPending) {
    return <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
  }
  if (request.isError) return <Failure error={request.error} />

  const data = request.data
  const names = new Map((workspaces.data ?? []).map((workspace) => [workspace.id, workspace.name]))
  const workspaceName = (id: string) => names.get(id) ?? (id === "personal" ? "Personal" : id)
  const device = `${data.client_label} (${data.os})`
  if (data.state === "approved" || data.state === "consumed") {
    return (
      <Outcome
        icon={<CheckCircleIcon className="size-8 text-white" />}
        title={data.enroll ? `Added to ${workspaceName(data.workspace_id)}` : "Signed in"}
      >
        {data.enroll && data.admission === "pending"
          ? `${data.client_label} is waiting for an owner or admin of ${workspaceName(data.workspace_id)} to accept it. `
          : ""}
        Return to your terminal; {data.client_label} is {data.enroll ? "added" : "signed in"} as {data.account.email}.
        You can close this tab.
      </Outcome>
    )
  }
  if (data.state === "denied") {
    return (
      <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="Sign-in denied">
        {device} was not signed in. Run pb login again if this was a mistake.
      </Outcome>
    )
  }
  if (data.state === "expired") return <Expired />

  return (
    <Pending
      data={data}
      device={device}
      workspaces={workspaces.data ?? []}
      workspaceName={workspaceName}
      busy={decide.isPending}
      error={decide.isError ? decide.error.message : undefined}
      onDecide={(decision, workspaceId) => decide.mutate({ decision, workspaceId })}
    />
  )
}

function Pending({
  data,
  device,
  workspaces,
  workspaceName,
  busy,
  error,
  onDecide,
}: {
  data: DeviceRequest
  device: string
  workspaces: Workspace[]
  workspaceName: (id: string) => string
  busy: boolean
  error: string | undefined
  onDecide: (decision: "approve" | "deny", workspaceId?: string) => void
}) {
  const [choice, setChoice] = useState<string>()
  const asked = workspaces.some((workspace) => workspace.id === data.requested_workspace)
  const workspaceId = choice ?? (asked ? data.requested_workspace : "personal")
  const machine = data.enrolled_machine

  return (
    <div className="flex flex-col gap-5">
      <Heading
        title={data.enroll ? "Add this computer to Paperboat?" : "Sign in again?"}
        text={
          <>
            <span className="font-medium text-white">{device}</span>{" "}
            {data.enroll ? "wants to join your Paperboat account" : "wants to sign in to your Paperboat account"}{" "}
            <span className="font-medium text-white">{data.account.email}</span>.
          </>
        }
      />
      {data.enroll && (
        <Field>
          <FieldLabel className="text-white/85">Workspace</FieldLabel>
          <WorkspacePicker
            tone="brand"
            workspaces={workspaces}
            value={workspaceId}
            onChange={setChoice}
            disabled={busy}
          />
        </Field>
      )}
      {machine && (
        <Field>
          <FieldLabel className="text-white/85">Already added</FieldLabel>
          <p className="text-base">
            {machine.alias} · {machine.workspaces.map(workspaceName).join(", ")}
          </p>
        </Field>
      )}
      <Field>
        <FieldLabel className="text-white/85">Code</FieldLabel>
        <p className="font-mono text-2xl tracking-[0.2em]">{data.user_code}</p>
        <FieldDescription className="text-white/75">
          Approve only if this matches the code in your terminal.
        </FieldDescription>
      </Field>
      {error && <p className="text-sm font-medium text-white">{error}</p>}
      <div className="flex gap-2">
        <Button
          size="lg"
          className={`flex-1 ${brandPrimary}`}
          disabled={busy}
          onClick={() => onDecide("approve", data.enroll ? workspaceId : undefined)}
        >
          {data.enroll ? "Add" : "Sign in"}
        </Button>
        <Button
          size="lg"
          variant="secondary"
          className={`flex-1 ${brandSecondary}`}
          disabled={busy}
          onClick={() => onDecide("deny")}
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
