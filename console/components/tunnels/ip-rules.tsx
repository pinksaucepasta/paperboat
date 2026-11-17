"use client"

import { XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { useApiQuery } from "@/lib/api"
import { ipRulesQuery, saveIpRules, type IPRules as Rules } from "@/lib/tunnels"
import { LoadError } from "@/components/load-error"
import { Section } from "@/components/section"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage } from "@/components/error-reference"

/** A tunnel's or preview's IP rules: who may reach it by address, checked by the edge before your app. */
export function IpRules({ kind, id }: { kind: "tunnel" | "preview"; id: string }) {
  const query = useApiQuery(ipRulesQuery(kind, id))
  if (query.isError)
    return (
      <Section title="IP rules">
        <LoadError what="IP rules" error={query.error} retry={() => query.refetch()} />
      </Section>
    )
  if (!query.data)
    return (
      <Section title="IP rules">
        <div className="flex flex-col gap-4 pt-3">
          {["Allow", "Deny"].map((label) => (
            <Field key={label} className="gap-1.5">
              <FieldLabel>{label}</FieldLabel>
              <Skeleton className="h-8 w-full" />
            </Field>
          ))}
        </div>
      </Section>
    )
  // A saved list remounts the form, so its draft starts from what the server holds.
  return <RulesForm key={JSON.stringify(query.data)} kind={kind} id={id} saved={query.data} />
}

function RulesForm({ kind, id, saved }: { kind: "tunnel" | "preview"; id: string; saved: Rules }) {
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState<Rules>({ allow: saved.allow ?? [], deny: saved.deny ?? [] })
  const dirty = JSON.stringify(draft) !== JSON.stringify({ allow: saved.allow ?? [], deny: saved.deny ?? [] })
  const save = useMutation({
    mutationFn: () => saveIpRules(kind, id, draft),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["ip-rules", kind, id] })
      toast.success("IP rules saved")
    },
  })
  return (
    <Section
      title="IP rules"
      action={
        <Button size="lg" disabled={!dirty || save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      }
    >
      <div className="flex flex-col gap-4 pt-3">
        <ChipField
          label="Allow"
          hint="Only these addresses can open it. Leave empty to allow everyone."
          values={draft.allow}
          onChange={(allow) => setDraft({ ...draft, allow })}
        />
        <ChipField
          label="Deny"
          hint="These addresses are always refused."
          values={draft.deny}
          onChange={(deny) => setDraft({ ...draft, deny })}
        />
        {save.isError && <FieldError>
            <ErrorMessage error={save.error} message={save.error.message.replace(/^ip rules invalid: /, "")} />
          </FieldError>}
      </div>
    </Section>
  )
}

/** Addresses or CIDR ranges as chips: Enter, comma, space or leaving the field adds what's typed. */
function ChipField({
  label,
  hint,
  values,
  onChange,
}: {
  label: string
  hint: string
  values: string[]
  onChange: (values: string[]) => void
}) {
  const [typed, setTyped] = useState("")
  const commit = () => {
    const entries = typed.split(/[\s,]+/).filter(Boolean)
    setTyped("")
    if (entries.length) onChange([...new Set([...values, ...entries])])
  }
  const field = `ip-rules-${label.toLowerCase()}`
  return (
    <Field className="gap-1.5">
      <FieldLabel htmlFor={field}>{label}</FieldLabel>
      <div className="flex min-h-8 flex-wrap items-center gap-1.5 rounded-md bg-surface px-2 py-1">
        {values.map((value) => (
          <Badge key={value} variant="outline" className="gap-1 font-mono font-normal text-code">
            {value}
            <button
              type="button"
              aria-label={`Remove ${value}`}
              onClick={() => onChange(values.filter((entry) => entry !== value))}
              className="text-muted-foreground outline-none hover:text-foreground"
            >
              <XIcon className="size-3" />
            </button>
          </Badge>
        ))}
        <Input
          id={field}
          value={typed}
          placeholder={values.length ? "" : "203.0.113.7 or 10.0.0.0/8"}
          onChange={(event) => setTyped(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" || event.key === "," || event.key === " ") {
              event.preventDefault()
              commit()
            } else if (event.key === "Backspace" && !typed && values.length) onChange(values.slice(0, -1))
          }}
          onBlur={commit}
          spellCheck={false}
          autoComplete="off"
          className="h-6 min-w-40 flex-1 border-0 bg-transparent px-1 font-mono shadow-none focus-visible:ring-0 dark:bg-transparent md:text-code"
        />
      </div>
      <p className="text-xs text-muted-foreground">{hint}</p>
    </Field>
  )
}
