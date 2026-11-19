"use client"

import { PencilSimpleIcon, PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { actionLabels, machineActions, type Policy } from "@/lib/policy"
import { member, parseTree, setItem, valueOf } from "@/lib/jsonc-edit"
import { CodeArea } from "@/components/access/policy-editor"
import { PolicyTest } from "@/components/access/policy-test"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { useShellState } from "@/lib/use-shell-state"

type Rule = {
  src: string[]
  dst: string[]
  allow?: string[]
  deny?: string[]
  ports?: (number | string)[]
  note?: string
}
const known = ["src", "dst", "allow", "deny", "ports", "note"]
const list = (v: unknown): v is string[] => Array.isArray(v) && v.every((x) => typeof x === "string")

/**
 * The rules of the file as the GUI can show them, or why it cannot. A rule with a key the GUI does not
 * know, or a shape it cannot edit, stops the table: the file view shows everything.
 */
export function readRules(text: string): { rules: Rule[] } | { problem: string } {
  const root = parseTree(text)
  if (!root || root.kind !== "object") return { problem: "The file is not valid yet." }
  const array = member(root, "rules")?.value
  if (!array) return { rules: [] }
  if (array.kind !== "array") return { problem: "“rules” is not a list." }
  const rules: Rule[] = []
  for (const [index, item] of array.items.entries()) {
    const rule = item.kind === "object" ? (valueOf(text, item) as Record<string, unknown>) : undefined
    const extra = rule && Object.keys(rule).find((key) => !known.includes(key))
    const ports = rule?.ports
    const portsOk =
      ports === undefined ||
      (Array.isArray(ports) && ports.every((p) => typeof p === "number" || typeof p === "string"))
    if (
      !rule ||
      extra ||
      !list(rule.src) ||
      !list(rule.dst) ||
      !portsOk ||
      (rule.allow !== undefined && !list(rule.allow)) ||
      (rule.deny !== undefined && !list(rule.deny)) ||
      (rule.note !== undefined && typeof rule.note !== "string")
    ) {
      return {
        problem: `Rule ${index + 1} ${extra ? `has a key, “${extra}”,` : "has a shape"} that the table cannot show.`,
      }
    }
    rules.push(rule as Rule)
  }
  return { rules }
}

const csv = (items: readonly (string | number)[] | undefined) => (items ?? []).join(", ")
const words = (text: string) => text.split(/[\s,]+/).filter(Boolean)
const named = (prefix: string, names: string[]) => names.map((name) => (name.startsWith(prefix) ? name : prefix + name))

/** Who → what → which machines, as a table over the same file text: every change edits the text in place. */
export function PolicyRules({
  workspace,
  policy,
  text,
  editable,
  onChange,
  openFile,
}: {
  workspace: string
  policy: Policy
  text: string
  editable: boolean
  onChange: (text: string) => void
  openFile: () => void
}) {
  const read = readRules(text)
  const [editing, setEditing] = useShellState<{ index: number; rule?: Rule }>("access/policy-rules.tsx:editing")
  if ("problem" in read) {
    return (
      <div className="flex flex-col items-start gap-3 surface rounded-lg p-4 text-sm" data-policy-rules="unsupported">
        <p>{read.problem} Edit this file as text.</p>
        <Button variant="outline" onClick={openFile}>
          Open the file
        </Button>
      </div>
    )
  }
  const { rules } = read
  return (
    <div className="flex flex-col gap-6" data-policy-rules="table">
      <section className="flex flex-col gap-2">
        <SectionHeading
          title="Rules"
          action={
            editable && (
              <Button size="lg" onClick={() => setEditing({ index: rules.length })}>
                <PlusIcon data-icon="inline-start" />
                Add rule
              </Button>
            )
          }
        />
        {rules.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted-foreground">No rules. Nobody but owners has access.</p>
        ) : (
          <ul className="flex flex-col border-t border-divider">
            <li aria-hidden className="hidden gap-3 py-1.5 text-xs text-muted-foreground md:flex">
              <div className="grid min-w-0 flex-1 gap-x-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)_minmax(0,1fr)]">
                <span>Who</span>
                <span>Access</span>
                <span>Machines and ports</span>
              </div>
              <span className="w-[4.5rem] shrink-0" />
            </li>
            {rules.map((rule, index) => {
              const deny = rule.deny !== undefined
              const actions = (rule.deny ?? rule.allow ?? []).map((a) => actionLabels[a] ?? a)
              return (
                <li key={index} className="flex items-start gap-3 border-b border-divider py-2.5 text-sm">
                  <div className="grid min-w-0 flex-1 gap-x-4 gap-y-0.5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)_minmax(0,1fr)]">
                    <span className="min-w-0 break-words">{rule.src.join(", ")}</span>
                    <span className="min-w-0">
                      <span className={deny ? "text-destructive" : "text-success"}>{deny ? "Deny" : "Allow"}</span>{" "}
                      {actions.join(", ")}
                    </span>
                    <span className="min-w-0 break-words text-muted-foreground">
                      {rule.dst.join(", ")}
                      {rule.ports?.length ? ` · ports ${csv(rule.ports)}` : ""}
                    </span>
                    {rule.note && <span className="text-xs text-muted-foreground md:col-span-3">{rule.note}</span>}
                  </div>
                  {editable && (
                    <div className="flex shrink-0 gap-1">
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Edit rule ${index + 1}`}
                        onClick={() => setEditing({ index, rule })}
                      >
                        <PencilSimpleIcon />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Delete rule ${index + 1}`}
                        onClick={() => onChange(setItem(text, "rules", index, undefined))}
                      >
                        <TrashIcon />
                      </Button>
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </section>
      <section className="flex flex-col gap-2">
        <SectionHeading title="Generated file" />
        <CodeArea value={text} compact />
      </section>
      <section className="flex flex-col gap-2">
        <SectionHeading title="Explain access" />
        <PolicyTest workspace={workspace} policy={policy} draft={text !== policy.policy ? text : undefined} />
      </section>
      {editing && (
        <RuleDialog
          policy={policy}
          text={text}
          initial={editing.rule}
          onClose={() => setEditing(undefined)}
          onSave={(rule) => {
            onChange(setItem(text, "rules", editing.index, rule))
            setEditing(undefined)
          }}
        />
      )}
    </div>
  )
}

function RuleDialog({
  policy,
  text,
  initial,
  onClose,
  onSave,
}: {
  policy: Policy
  text: string
  initial?: Rule
  onClose: () => void
  onSave: (rule: Rule) => void
}) {
  const [kind, setKind] = useShellState<"allow" | "deny">(
    "access/policy-rules.tsx:kind",
    initial?.deny ? "deny" : "allow",
  )
  const [who, setWho] = useShellState("access/policy-rules.tsx:who", csv(initial?.src))
  const [where, setWhere] = useShellState("access/policy-rules.tsx:where", csv(initial?.dst))
  const [actions, setActions] = useShellState<string[]>(
    "access/policy-rules.tsx:actions",
    initial?.deny ?? initial?.allow ?? ["terminal"],
  )
  const [ports, setPorts] = useShellState("access/policy-rules.tsx:ports", csv(initial?.ports))
  const [note, setNote] = useShellState("access/policy-rules.tsx:note", initial?.note ?? "")
  const [error, setError] = useState<string>()
  const groups = Object.keys((parseFile(text).groups as object) ?? {})
  const tags = Object.keys((parseFile(text).tags as object) ?? {})
  const privateOnly = actions.length === 1 && actions[0] === "private_access"

  function submit() {
    const src = words(who)
    const dst = words(where)
    if (!src.length) return setError("Say who the rule is for.")
    if (!dst.length) return setError("Say which machines it covers.")
    if (!actions.length) return setError("Choose at least one action.")
    const rule: Rule = { src, dst, [kind]: actions }
    const parsed = words(ports).map((p) => (/^\d+$/.test(p) ? Number(p) : p))
    if (privateOnly && parsed.length) rule.ports = parsed
    if (note.trim()) rule.note = note.trim()
    onSave(rule)
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>{initial ? "Edit rule" : "Add rule"}</DialogTitle>
        <ToggleGroup
          aria-label="Access"
          value={[kind]}
          onValueChange={(next) => next[0] && setKind(next[0] as "allow" | "deny")}
          variant="outline"
          spacing={2}
        >
          <ToggleGroupItem value="allow">Allow</ToggleGroupItem>
          <ToggleGroupItem value="deny">Deny</ToggleGroupItem>
        </ToggleGroup>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="rule-who">Who</FieldLabel>
          <Input
            id="rule-who"
            list="rule-who-names"
            value={who}
            autoComplete="off"
            spellCheck={false}
            placeholder="group:platform, user:ana@example.test"
            onChange={(e) => setWho(e.target.value)}
          />
          <datalist id="rule-who-names">
            {[...named("user:", policy.names.users), ...named("group:", groups), "role:admin", "role:member", "*"].map(
              (n) => (
                <option key={n} value={n} />
              ),
            )}
          </datalist>
          <span className="text-xs text-muted-foreground">Separate with commas.</span>
        </Field>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="rule-where">Machines</FieldLabel>
          <Input
            id="rule-where"
            list="rule-where-names"
            value={where}
            autoComplete="off"
            spellCheck={false}
            placeholder="tag:ci, machine:db1"
            onChange={(e) => setWhere(e.target.value)}
          />
          <datalist id="rule-where-names">
            {[
              ...named("tag:", tags.length ? tags : policy.names.tags),
              ...named("machine:", policy.names.machines),
              "machine:*",
            ].map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
        </Field>
        <Field className="gap-1.5">
          <FieldLabel>Actions</FieldLabel>
          <div className="grid grid-cols-2 gap-x-3 gap-y-1.5">
            {machineActions.map((action) => (
              <label key={action} className="flex items-center gap-2 text-sm">
                <Checkbox
                  checked={actions.includes(action)}
                  onCheckedChange={(on) => setActions(on ? [...actions, action] : actions.filter((a) => a !== action))}
                />
                {actionLabels[action]}
              </label>
            ))}
          </div>
        </Field>
        {privateOnly && (
          <Field className="gap-1.5">
            <div className="flex items-center justify-between gap-2">
              <FieldLabel htmlFor="rule-ports">Ports</FieldLabel>
              <span className="text-xs text-muted-foreground">Optional</span>
            </div>
            <Input
              id="rule-ports"
              value={ports}
              spellCheck={false}
              placeholder="5432, 8000-8100"
              onChange={(e) => setPorts(e.target.value)}
            />
          </Field>
        )}
        <Field className="gap-1.5">
          <div className="flex items-center justify-between gap-2">
            <FieldLabel htmlFor="rule-note">Note</FieldLabel>
            <span className="text-xs text-muted-foreground">Optional</span>
          </div>
          <Input id="rule-note" value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
        {error && <FieldError>{error}</FieldError>}
        <DialogFooter>
          <Button variant="outline" size="lg" onClick={onClose}>
            Cancel
          </Button>
          <Button size="lg" onClick={submit}>
            {initial ? "Update rule" : "Add rule"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function parseFile(text: string): Record<string, unknown> {
  const root = parseTree(text)
  return root?.kind === "object" ? (valueOf(text, root) as Record<string, unknown>) : {}
}
