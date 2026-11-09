"use client"

import { CheckCircleIcon, KeyIcon, SlidersHorizontalIcon, WarningIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { clientConfigQuery, useApiQuery, useMachines, useWorkspace } from "@/lib/api"
import { actionLabels, createApiToken, machineActions, namePattern, PolicyError, policyQuery } from "@/lib/policy"
import { queryKeys } from "@/lib/query-keys"
import {
  actionNames,
  dateInDays,
  daysUntil,
  expiryChoices,
  permissionDetails,
  targetName,
  tokenScope,
} from "@/lib/token-scope"
import { useNow } from "@/lib/use-now"
import { useTimeFormat } from "@/lib/use-time-format"
import { TokenMachines } from "@/components/access/token-machines"
import { CommandBlock } from "@/components/add-machine-dialog"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Field } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

const listPath = "/settings/api-tokens"
const selectClass =
  "h-9 rounded-md border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"

type Mode = "full" | "custom"
type Form = {
  name: string
  expiry: string
  customDate: string
  machines: "all" | "selected"
  targets: string[]
  actions: string[]
}

/** Create token: choose Full access or Custom, fill the form, check the summary, copy the token once. */
export function NewToken() {
  const [mode, setMode] = useState<Mode | null>(null)
  const [step, setStep] = useState<"form" | "summary">("form")
  const [form, setForm] = useState<Form>({
    name: "",
    expiry: "90",
    customDate: "",
    machines: "all",
    targets: [],
    actions: [],
  })
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  const now = useNow(60_000)
  const days =
    form.expiry === "custom"
      ? form.customDate
        ? daysUntil(form.customDate, now)
        : 0
      : Number(form.expiry)
  const create = useMutation({
    mutationFn: () =>
      createApiToken(workspace?.id ?? "", {
        name: form.name,
        expires_in_days: days,
        ...(mode === "custom"
          ? { scope: tokenScope(form.machines === "all" ? "all" : form.targets, form.actions) }
          : {}),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.apiTokens(workspace?.id ?? "") }),
  })

  if (create.isSuccess) return <Created secret={create.data.secret} />
  if (!mode) return <Start onChoose={setMode} />
  const valid =
    namePattern.test(form.name) &&
    days > 0 &&
    (mode === "full" || (form.actions.length > 0 && (form.machines === "all" || form.targets.length > 0)))
  const expires = now > 0 && days > 0 ? new Date(now + days * 86_400_000) : null
  if (step === "summary")
    return (
      <Summary
        form={form}
        mode={mode}
        expires={expires}
        pending={create.isPending}
        error={create.error}
        onBack={() => {
          create.reset()
          setStep("form")
        }}
        onCreate={() => create.mutate()}
      />
    )
  return (
    <TokenForm
      form={form}
      mode={mode}
      now={now}
      expires={expires}
      valid={valid}
      onChange={(next) => setForm({ ...form, ...next })}
      onContinue={() => setStep("summary")}
    />
  )
}

function Start({ onChoose }: { onChoose: (mode: Mode) => void }) {
  const choices = [
    { mode: "full" as const, icon: KeyIcon, title: "Full access", detail: "Acts as you: everything you can do in this workspace." },
    { mode: "custom" as const, icon: SlidersHorizontalIcon, title: "Custom", detail: "Choose the machines and what the token may do on them." },
  ]
  return (
    <div className="flex flex-col gap-3">
      <SectionHeading title="Create token" />
      <div className="grid gap-3 md:grid-cols-2">
        {choices.map(({ mode, icon: Icon, title, detail }) => (
          <button
            key={mode}
            type="button"
            onClick={() => onChoose(mode)}
            className="surface flex flex-col items-start gap-3 rounded-lg p-5 text-left outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30"
          >
            <Icon className="size-5 text-muted-foreground" />
            <span className="flex flex-col gap-1">
              <span className="text-sm font-medium">{title}</span>
              <span className="text-sm text-muted-foreground">{detail}</span>
            </span>
          </button>
        ))}
      </div>
      <div>
        <Button variant="outline" size="lg" render={<Link href={listPath} />}>
          Cancel
        </Button>
      </div>
    </div>
  )
}

/** A form section: a title and one muted line, then its controls. */
function Block({ title, hint, children }: { title: string; hint: string; children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-col gap-1">
        <SectionHeading title={title} />
        <p className="text-sm text-muted-foreground">{hint}</p>
      </div>
      <div className="flex flex-col gap-3 border-t border-divider pt-4">{children}</div>
    </section>
  )
}

function TokenForm({
  form,
  mode,
  now,
  expires,
  valid,
  onChange,
  onContinue,
}: {
  form: Form
  mode: Mode
  now: number
  expires: Date | null
  valid: boolean
  onChange: (next: Partial<Form>) => void
  onContinue: () => void
}) {
  const time = useTimeFormat()
  return (
    <form
      className="flex flex-col gap-8"
      onSubmit={(event) => {
        event.preventDefault()
        if (valid) onContinue()
      }}
    >
      <Block title="Name" hint="What the token is for, so you can tell it apart later.">
        <Field className="max-w-md gap-1.5">
          <Input
            id="token-name"
            aria-label="Name"
            autoFocus
            placeholder="ci-runner"
            value={form.name}
            onChange={(event) => onChange({ name: event.target.value.toLowerCase() })}
            autoComplete="off"
            spellCheck={false}
          />
          <span className="text-xs text-muted-foreground">
            Lowercase letters, numbers, dots, dashes and underscores. Up to 63.
          </span>
        </Field>
      </Block>
      <Block title="Expiration" hint="The token stops working on this day.">
        <div className="flex max-w-md flex-col gap-1.5">
          <div className="flex gap-2">
            <select
              aria-label="Expires after"
              className={selectClass}
              value={form.expiry}
              onChange={(event) => onChange({ expiry: event.target.value })}
            >
              {expiryChoices.map((choice) => (
                <option key={choice.id} value={choice.id}>
                  {choice.label}
                </option>
              ))}
            </select>
            {form.expiry === "custom" && (
              <Input
                type="date"
                aria-label="Expiration date"
                className="w-44"
                min={now > 0 ? dateInDays(1, now) : undefined}
                max={now > 0 ? dateInDays(365, now) : undefined}
                value={form.customDate}
                onChange={(event) => onChange({ customDate: event.target.value })}
              />
            )}
          </div>
          <span className="min-h-5 text-xs text-muted-foreground">{expires && `Expires on ${time.date(expires.toISOString(), "date")}`}</span>
        </div>
      </Block>
      {mode === "custom" && (
        <>
          <MachinesBlock form={form} onChange={onChange} />
          <Block title="Permissions" hint="What the token may do on those machines.">
            <PermissionTable actions={form.actions} onChange={(actions) => onChange({ actions })} />
            <p className="text-xs text-muted-foreground">A token can never do more than you can.</p>
          </Block>
        </>
      )}
      <div className="flex gap-2">
        <Button variant="outline" size="lg" type="button" render={<Link href={listPath} />}>
          Cancel
        </Button>
        <Button size="lg" type="submit" disabled={!valid}>
          Continue to summary
        </Button>
      </div>
    </form>
  )
}

function MachinesBlock({ form, onChange }: { form: Form; onChange: (next: Partial<Form>) => void }) {
  const workspace = useWorkspace()
  const policy = useApiQuery(policyQuery(workspace?.id ?? ""))
  const machines = useMachines()
  const tags = policy.data?.names.tags ?? []
  const none = !machines.isPending && !policy.isPending && machines.data?.length === 0 && tags.length === 0
  const choices = [
    { id: "all" as const, label: "All machines", disabled: false },
    { id: "selected" as const, label: none ? "Only selected (no machines yet)" : "Only selected", disabled: none },
  ]
  return (
    <Block title="Machines" hint="Which machines the token can reach.">
      <div role="radiogroup" aria-label="Machines" className="flex flex-col gap-2 text-sm">
        {choices.map((choice) => (
          <label
            key={choice.id}
            className={`flex items-center gap-3 ${choice.disabled ? "text-muted-foreground" : "cursor-pointer"}`}
          >
            <input
              type="radio"
              name="token-machines"
              checked={form.machines === choice.id}
              disabled={choice.disabled}
              onChange={() => onChange({ machines: choice.id })}
            />
            {choice.label}
          </label>
        ))}
      </div>
      {form.machines === "selected" && (
        <div className="max-w-md">
          <TokenMachines tags={tags} names={policy.data?.names.machines ?? []} value={form.targets} onChange={(targets) => onChange({ targets })} />
        </div>
      )}
    </Block>
  )
}

/** One row per capability: name and what it does, then its access. The API has only allowed or not. */
function PermissionTable({ actions, onChange }: { actions: string[]; onChange: (actions: string[]) => void }) {
  return (
    <ul className="flex flex-col border-t border-divider">
      {machineActions.map((action) => (
        <li key={action} className="flex min-h-14 items-center justify-between gap-4 border-b border-divider py-2 text-sm">
          <span className="flex min-w-0 flex-col">
            <span className="font-medium">{actionLabels[action]}</span>
            <span className="text-muted-foreground">{permissionDetails[action]}</span>
          </span>
          <select
            aria-label={`${actionLabels[action]} access`}
            className={`${selectClass} w-36 shrink-0`}
            value={actions.includes(action) ? "allowed" : "none"}
            onChange={(event) =>
              onChange(event.target.value === "allowed" ? [...actions, action] : actions.filter((a) => a !== action))
            }
          >
            <option value="none">No access</option>
            <option value="allowed">Allowed</option>
          </select>
        </li>
      ))}
    </ul>
  )
}

function Summary({
  form,
  mode,
  expires,
  pending,
  error,
  onBack,
  onCreate,
}: {
  form: Form
  mode: Mode
  expires: Date | null
  pending: boolean
  error: Error | null
  onBack: () => void
  onCreate: () => void
}) {
  const time = useTimeFormat()
  const machines =
    form.machines === "all" ? "All machines" : form.targets.map(targetName).join(", ")
  const rows = [
    ["Name", form.name],
    ["Expires", expires ? time.date(expires.toISOString(), "date") : ""],
    ...(mode === "custom"
      ? [
          ["Machines", machines],
          ["Permissions", actionNames(form.actions)],
        ]
      : [["Permissions", "Everything you can do"]]),
  ]
  const problems = error instanceof PolicyError ? (error.details.errors ?? []) : []
  return (
    <div className="flex flex-col gap-4">
      <SectionHeading title="Summary" />
      <dl className="surface flex flex-col rounded-lg px-4 py-1 text-sm">
        {rows.map(([label, value]) => (
          <div key={label} className="flex flex-col gap-1 border-b border-divider py-3 last:border-b-0 sm:flex-row sm:gap-6">
            <dt className="w-32 shrink-0 text-muted-foreground">{label}</dt>
            <dd className="min-w-0 break-words">{value}</dd>
          </div>
        ))}
      </dl>
      {error && <p className="text-sm text-destructive">{error.message}</p>}
      {problems.length > 0 && (
        <ul className="text-sm text-destructive">
          {problems.map((problem, index) => (
            <li key={index}>
              <span className="font-mono text-xs">{problem.path}</span> {problem.message}
            </li>
          ))}
        </ul>
      )}
      <div className="flex gap-2">
        <Button variant="outline" size="lg" disabled={pending} onClick={onBack}>
          Back
        </Button>
        <Button size="lg" disabled={pending} onClick={onCreate}>
          {pending ? "Creating…" : "Create token"}
        </Button>
      </div>
    </div>
  )
}

function Created({ secret }: { secret: string }) {
  const api = useApiQuery(clientConfigQuery).data?.issuer
  return (
    <div className="flex flex-col gap-4">
      <SectionHeading title="Token created" />
      <p className="flex items-center gap-2 text-sm">
        <WarningIcon className="size-4 shrink-0 text-muted-foreground" />
        Copy it now — you won’t see it again.
      </p>
      <div data-token-secret>
        <CommandBlock command={secret} />
      </div>
      {api && (
        <div className="flex flex-col gap-2 border-t border-divider pt-4">
          <span className="flex items-center gap-2 text-sm text-muted-foreground">
            <CheckCircleIcon className="size-4" />
            Try it
          </span>
          <CommandBlock command={`curl -H "Authorization: Bearer ${secret}" ${api}/v1/machines`} />
        </div>
      )}
      <div>
        <Button size="lg" render={<Link href={listPath} />}>
          Done
        </Button>
      </div>
    </div>
  )
}
