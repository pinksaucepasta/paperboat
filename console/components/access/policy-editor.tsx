"use client"

import { useQuery } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import {
  hasComments,
  parsePolicy,
  readOnlyNote,
  savePolicy,
  validatePolicy,
  type Policy,
  type Validation,
} from "@/lib/policy"
import { ChangesDialog } from "@/components/access/changes-dialog"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"

/** The policy as a file: a plain editor with line numbers, checked by the server as you type. */
export function PolicyEditor({
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
  const [checked, setChecked] = useState(text)
  const [reviewing, setReviewing] = useState(false)
  useEffect(() => {
    const timer = setTimeout(() => setChecked(text), 800)
    return () => clearTimeout(timer)
  }, [text])
  const validation = useQuery({
    queryKey: ["policy-validate", workspace, checked],
    queryFn: () => validatePolicy(workspace, checked),
    staleTime: Infinity,
    retry: false,
  })
  const dirty = text !== policy.policy
  const formattable = parsePolicy(text) !== undefined && !hasComments(text)
  const note = readOnlyNote(policy)

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
        <span className="flex-1">
          Version {policy.version} · saved <Time iso={policy.updated_at} relative /> by{" "}
          {policy.updated_by.replace(/^user:/, "")}
          {policy.updated_with_api_token && ` with API token ${policy.updated_with_api_token}`}
        </span>
        {editable && (
          <>
            <Button
              variant="outline"
              disabled={!formattable}
              title={formattable ? undefined : "Format works on a file without comments that parses"}
              onClick={() => onChange(JSON.stringify(parsePolicy(text), null, 2) + "\n")}
            >
              Format
            </Button>
            <Button variant="outline" onClick={() => (checked === text ? validation.refetch() : setChecked(text))}>
              Validate
            </Button>
            <Button variant="outline" disabled={!dirty} onClick={() => onChange(policy.policy)}>
              Discard
            </Button>
            <Button size="lg" disabled={!dirty} onClick={() => setReviewing(true)}>
              Review changes
            </Button>
          </>
        )}
      </div>
      {note && <p className="text-sm text-muted-foreground">{note}</p>}
      <CodeArea value={text} onChange={editable ? onChange : undefined} />
      <ValidationResult
        validation={validation.data}
        error={validation.error}
        checking={validation.isFetching || checked !== text}
      />
      {reviewing && (
        <ChangesDialog
          workspace={workspace}
          title="Review changes"
          verb="Save"
          review={() => validatePolicy(workspace, text)}
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

function ValidationResult({
  validation,
  error,
  checking,
}: {
  validation: Validation | undefined
  error: Error | null
  checking: boolean
}) {
  if (error) return <p className="text-sm text-destructive">{error.message}</p>
  if (!validation) return <p className="text-sm text-muted-foreground">Checking…</p>
  const { passed, failed } = validation.tests
  return (
    <div className="flex flex-col gap-2 text-sm" data-policy-validation={validation.valid ? "valid" : "invalid"}>
      <p className={validation.valid ? "text-muted-foreground" : "text-destructive"}>
        {checking ? "Checking… " : ""}
        {validation.valid
          ? "No errors."
          : `${validation.errors.length} ${validation.errors.length === 1 ? "error" : "errors"}.`}
        {passed + failed > 0 && ` Tests: ${passed} passed${failed ? `, ${failed} failed` : ""}.`}
      </p>
      {validation.errors.length > 0 && (
        <ul className="flex flex-col border-t border-divider">
          {validation.errors.map((problem, index) => (
            <li key={index} className="flex flex-wrap items-baseline gap-x-3 border-b border-divider py-1.5">
              <span className="font-mono text-xs">{problem.path || "/"}</span>
              <span className="font-mono text-xs text-muted-foreground">{problem.code}</span>
              <span className="min-w-0 flex-1">{problem.message}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** A monospace textarea with a line-number gutter that scrolls with it. Read-only without `onChange`. */
export function CodeArea({ value, onChange }: { value: string; onChange?: (value: string) => void }) {
  const gutter = useRef<HTMLDivElement>(null)
  const lines = value.split("\n").length
  return (
    <div className="flex h-[32rem] overflow-hidden surface rounded-lg font-mono text-xs leading-5">
      <div
        ref={gutter}
        aria-hidden
        className="shrink-0 overflow-hidden py-3 pr-3 pl-3 text-right text-muted-foreground tabular-nums select-none"
      >
        {Array.from({ length: lines }, (_, i) => (
          <div key={i}>{i + 1}</div>
        ))}
      </div>
      <textarea
        aria-label="Policy file"
        className="min-w-0 flex-1 resize-none bg-transparent py-3 pr-3 whitespace-pre outline-none"
        value={value}
        readOnly={!onChange}
        spellCheck={false}
        autoCapitalize="off"
        autoComplete="off"
        wrap="off"
        onChange={(event) => onChange?.(event.target.value)}
        onScroll={(event) => {
          if (gutter.current) gutter.current.scrollTop = event.currentTarget.scrollTop
        }}
      />
    </div>
  )
}
