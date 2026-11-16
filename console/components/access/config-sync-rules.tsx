"use client"

import { PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { hasComments, parsePolicy, savePolicy, validatePolicy, type ConfigSyncRule, type Policy } from "@/lib/policy"
import { configSyncProblems } from "@/lib/config-sync"
import { ChangesDialog } from "@/components/access/changes-dialog"
import { usePolicyValidation } from "@/components/access/policy-editor"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/**
 * The policy's `config_sync` rules as rows: which repository machines (all, or by tag) sync config with.
 * Edits the same draft as the Policy file tab and saves through the same review.
 */
export function ConfigSyncRules({
  workspace,
  policy,
  text,
  onChange,
  onSaved,
}: {
  workspace: string
  policy: Policy
  text: string
  onChange: (text: string) => void
  onSaved: () => void
}) {
  const editable = policy.can_edit === "all"
  const [reviewing, setReviewing] = useState(false)
  const { validation } = usePolicyValidation(workspace, text)
  const file = parsePolicy(text)
  // Re-writing the file as rows would drop its comments, so a commented file is edited as a file.
  const rows = file?.config_sync ?? []
  const locked = !editable || !file || hasComments(text)
  const set = (next: ConfigSyncRule[]) => {
    if (!file) return
    // JSON.stringify leaves out an undefined list, so removing the last rule removes the section.
    onChange(JSON.stringify({ ...file, config_sync: next.length ? next : undefined }, null, 2) + "\n")
  }
  const change = (index: number, patch: Partial<ConfigSyncRule>) =>
    set(rows.map((row, i) => (i === index ? { ...row, ...patch } : row)))
  const problems = (index: number) =>
    (validation.data?.errors ?? []).filter((problem) => problem.path.startsWith(`/config_sync/${index}`))
  const dirty = text !== policy.policy

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-end gap-2">
        {!locked && (
          <Button variant="outline" size="lg" onClick={() => set([...rows, { dst: ["machine:*"], repository: "" }])}>
            <PlusIcon data-icon="inline-start" />
            Add rule
          </Button>
        )}
        {editable && (
          <Button size="lg" disabled={!dirty} onClick={() => setReviewing(true)}>
            Review changes
          </Button>
        )}
      </div>
      {file && hasComments(text) && (
        <p className="text-sm text-muted-foreground">The policy file has comments. Edit these rules in Policy file.</p>
      )}
      {!file && <p className="text-sm text-muted-foreground">The policy file doesn’t parse. Fix it in Policy file.</p>}
      {rows.length === 0 ? (
        <p className="border-y border-divider py-3 text-sm text-muted-foreground">
          No rules. Machines choose their own repository.
        </p>
      ) : (
        rows.map((row, index) => (
          <div key={index} className="flex flex-col gap-3 border-b border-divider pb-4">
            <div className="grid gap-3 sm:grid-cols-[1fr_2fr_8rem]">
              <Field className="gap-1.5">
                <FieldLabel htmlFor={`cs-target-${index}`}>Machines</FieldLabel>
                <Input
                  id={`cs-target-${index}`}
                  value={row.dst.join(", ")}
                  disabled={locked}
                  placeholder="machine:* or tag:work"
                  onChange={(event) =>
                    change(index, {
                      dst: event.target.value
                        .split(",")
                        .map((target) => target.trim())
                        .filter(Boolean),
                    })
                  }
                />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor={`cs-repo-${index}`}>Repository</FieldLabel>
                <Input
                  id={`cs-repo-${index}`}
                  value={row.repository}
                  disabled={locked}
                  placeholder="https://… or ssh://… or /path"
                  onChange={(event) => change(index, { repository: event.target.value })}
                />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor={`cs-branch-${index}`}>
                  Branch <span className="ml-auto font-normal text-muted-foreground">Optional</span>
                </FieldLabel>
                <Input
                  id={`cs-branch-${index}`}
                  value={row.branch ?? ""}
                  disabled={locked}
                  placeholder="main"
                  onChange={(event) => change(index, { branch: event.target.value || undefined })}
                />
              </Field>
            </div>
            <div className="flex items-end gap-3">
              <Field className="flex-1 gap-1.5">
                <FieldLabel htmlFor={`cs-note-${index}`}>
                  Note <span className="ml-auto font-normal text-muted-foreground">Optional</span>
                </FieldLabel>
                <Input
                  id={`cs-note-${index}`}
                  value={row.note ?? ""}
                  disabled={locked}
                  onChange={(event) => change(index, { note: event.target.value || undefined })}
                />
              </Field>
              {!locked && (
                <Button
                  variant="outline"
                  size="lg"
                  aria-label="Remove rule"
                  onClick={() => set(rows.filter((_, i) => i !== index))}
                >
                  <TrashIcon />
                </Button>
              )}
            </div>
            {problems(index).map((problem, i) => (
              <p key={i} className="text-sm text-destructive">
                {configSyncProblems[problem.code] ?? problem.message}
              </p>
            ))}
          </div>
        ))
      )}
      {reviewing && (
        <ChangesDialog
          workspace={workspace}
          title="Review changes"
          verb="Save"
          review={(signal) => validatePolicy(workspace, text, signal)}
          run={(fields) => savePolicy(workspace, { policy: text, expected_version: policy.version, ...fields })}
          onClose={() => setReviewing(false)}
          onDone={() => {
            setReviewing(false)
            onSaved()
          }}
        />
      )}
    </div>
  )
}
