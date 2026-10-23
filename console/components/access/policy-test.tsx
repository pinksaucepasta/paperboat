"use client"

import { useMutation, useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { meQuery, useApiQuery, useWorkspace } from "@/lib/api"
import {
  actionLabels,
  apiTokensQuery,
  explainPolicy,
  guardrailLabels,
  machineActions,
  reasonLabels,
  validatePolicy,
  type Policy,
} from "@/lib/policy"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

const selectClass =
  "h-9 rounded-md border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"

/** "May this person do this here, and why": explain against the saved policy or the unsaved draft. */
export function PolicyTest({ workspace, policy, draft }: { workspace: string; policy: Policy; draft?: string }) {
  const role = useWorkspace()?.role
  const me = useApiQuery(meQuery).data
  const tokens = useApiQuery(apiTokensQuery(workspace))
  const admin = role === "owner" || role === "admin"
  const users = admin ? policy.names.users : me ? [me.email] : []
  const team = workspace !== "personal"
  const targets = [
    ...policy.names.machines.map((name) => `machine:${name}`),
    ...policy.names.previews.map((name) => `preview:${name}`),
    ...policy.names.tunnels.map((name) => `tunnel:${name}`),
    ...(team ? ["env:team"] : []),
  ]
  const [src, setSrc] = useState("")
  const [dst, setDst] = useState("")
  const [action, setAction] = useState("terminal")
  const [port, setPort] = useState("")
  const [token, setToken] = useState("")
  const [useDraft, setUseDraft] = useState(Boolean(draft))
  const who = src || users[0] || ""
  const where = dst || targets[0] || ""
  const actions =
    where === "env:team"
      ? ["env_read", "env_write"]
      : where.startsWith("machine:")
        ? machineActions
        : ["inspect", "replay"]
  const chosen = (actions as readonly string[]).includes(action) ? action : actions[0]
  const portNumber = Number(port)
  const portValid =
    chosen !== "private_access" ||
    port === "" ||
    (Number.isInteger(portNumber) && portNumber >= 1 && portNumber <= 65535)
  const text = useDraft && draft ? draft : policy.policy

  const explain = useMutation({
    mutationFn: () =>
      explainPolicy(workspace, {
        ...(useDraft && draft ? { policy: draft } : {}),
        src: `user:${who}`,
        dst: where,
        action: chosen,
        ...(chosen === "private_access" && port ? { port: portNumber } : {}),
        ...(token ? { api_token: token } : {}),
      }),
  })
  const tests = useQuery({
    queryKey: ["policy-validate", workspace, text],
    queryFn: () => validatePolicy(workspace, text),
    staleTime: Infinity,
    retry: false,
  })
  const decision = explain.data?.decision

  return (
    <div className="flex flex-col gap-10">
      <form
        className="grid gap-4 sm:grid-cols-2"
        onSubmit={(event) => {
          event.preventDefault()
          if (who && where && portValid) explain.mutate()
        }}
      >
        <Field className="gap-1.5">
          <FieldLabel htmlFor="test-who">Who</FieldLabel>
          <select id="test-who" className={selectClass} value={who} onChange={(event) => setSrc(event.target.value)}>
            {users.map((user) => (
              <option key={user} value={user}>
                {user}
              </option>
            ))}
          </select>
        </Field>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="test-target">Machine</FieldLabel>
          <select
            id="test-target"
            className={selectClass}
            value={where}
            onChange={(event) => setDst(event.target.value)}
          >
            {targets.length === 0 && <option value="">No machines yet</option>}
            {targets.map((target) => (
              <option key={target} value={target}>
                {target.replace(/^machine:/, "")}
              </option>
            ))}
          </select>
        </Field>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="test-action">Action</FieldLabel>
          <select
            id="test-action"
            className={selectClass}
            value={chosen}
            onChange={(event) => setAction(event.target.value)}
          >
            {actions.map((id) => (
              <option key={id} value={id}>
                {actionLabels[id] ?? id}
              </option>
            ))}
          </select>
        </Field>
        {chosen === "private_access" && (
          <Field className="gap-1.5">
            <FieldLabel htmlFor="test-port" className="flex justify-between">
              Port <span className="font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="test-port"
              inputMode="numeric"
              placeholder="5432"
              value={port}
              onChange={(event) => setPort(event.target.value.trim())}
            />
            {!portValid && <span className="text-xs text-destructive">A port from 1 to 65535.</span>}
          </Field>
        )}
        {tokens.isSuccess && tokens.data.some((t) => !t.revoked_at) && (
          <Field className="gap-1.5">
            <FieldLabel htmlFor="test-token" className="flex justify-between">
              API token <span className="font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <select
              id="test-token"
              className={selectClass}
              value={token}
              onChange={(event) => setToken(event.target.value)}
            >
              <option value="">None</option>
              {tokens.data
                .filter((t) => !t.revoked_at)
                .map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
            </select>
          </Field>
        )}
        <div className="flex flex-wrap items-center gap-3 sm:col-span-2">
          {draft && (
            <label className="flex items-center gap-2 text-sm">
              <Checkbox checked={useDraft} onCheckedChange={(on) => setUseDraft(Boolean(on))} />
              Test your unsaved changes
            </label>
          )}
          <Button
            type="submit"
            size="lg"
            className="ml-auto"
            disabled={!who || !where || !portValid || explain.isPending}
          >
            {explain.isPending ? "Testing…" : "Test"}
          </Button>
        </div>
      </form>

      {explain.isError && <p className="text-sm text-destructive">{explain.error.message}</p>}
      {decision && (
        <div
          className="flex flex-col gap-1 surface rounded-lg p-4 text-sm"
          data-decision={decision.allowed ? "allowed" : "denied"}
        >
          <span
            className={
              decision.allowed ? "text-lg font-semibold text-success" : "text-lg font-semibold text-destructive"
            }
          >
            {decision.allowed ? "Allowed" : "Denied"}
          </span>
          {decision.allowed && decision.key_delivered === false && (
            <span data-key-delivered="false">
              The team ENV key isn’t delivered to them yet: a key holder runs{" "}
              <code className="font-mono text-code">pb env grants sync</code>.
            </span>
          )}
          <span>
            {decision.guardrail
              ? (guardrailLabels[decision.guardrail] ?? decision.guardrail)
              : (reasonLabels[decision.reason] ?? decision.reason)}
          </span>
          {decision.rules && decision.rules.length > 0 && (
            <span className="font-mono text-xs text-muted-foreground">{decision.rules.join(", ")}</span>
          )}
        </div>
      )}

      <Section title={useDraft && draft ? "Tests in your unsaved file" : "Tests in the policy file"}>
        <div className="py-3 text-sm" data-policy-tests>
          {tests.isPending ? (
            <span className="text-muted-foreground">Running…</span>
          ) : tests.isError ? (
            <span className="text-destructive">{tests.error.message}</span>
          ) : tests.data.tests.passed + tests.data.tests.failed === 0 ? (
            <span className="text-muted-foreground">The file has no tests.</span>
          ) : (
            <>
              <span className={tests.data.tests.failed ? "text-destructive" : undefined}>
                {tests.data.tests.passed} passed, {tests.data.tests.failed} failed
              </span>
              <ul className="mt-2 flex flex-col">
                {tests.data.errors
                  .filter((problem) => problem.code === "test_failed")
                  .map((problem) => (
                    <li key={problem.path} className="flex gap-3 border-b border-divider py-1.5">
                      <span className="font-mono text-xs">{problem.path}</span>
                      <span>{problem.message}</span>
                    </li>
                  ))}
              </ul>
            </>
          )}
        </div>
      </Section>
    </div>
  )
}
