"use client"

import { PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import {
  patchRoute,
  tunnelKeys,
  type RouteHeaderRule,
  type RouteRedirect,
  type RouteRules,
  type Tunnel,
  type TunnelRoute,
} from "@/lib/tunnels"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

/*
 * Header and URL rules of one route, shown from the route's "Edit rules…" menu entry. The limits and the
 * list of headers Paperboat keeps for itself are the server's (shared/tunnelrouterules); the checks here only
 * save a round trip, the server answers with the same words.
 */

const MAX_HEADERS = 8
const MAX_REDIRECTS = 4
const BLOCKED_NAMES = new Set([
  "forwarded", "x-real-ip", "x-request-id", "host", "connection", "proxy-connection", "proxy-authenticate",
  "proxy-authorization", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "content-length", "cookie",
  "set-cookie", "authorization", "paperboat-visitor-assertion", "alt-svc", "strict-transport-security",
  "content-security-policy", "x-content-type-options", "x-frame-options", "referrer-policy", "x-robots-tag", "location",
  "x-forwarded", "x-paperboat", "paperboat", "sec-websocket",
])
const BLOCKED_PREFIXES = ["x-forwarded-", "x-paperboat-", "paperboat-", "sec-websocket-"]

function headerProblem(rule: RouteHeaderRule) {
  const name = rule.name.trim()
  if (!name) return "Enter a header name."
  if (!/^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/.test(name)) return "Use letters, digits and - _ . in the name."
  const lower = name.toLowerCase()
  if (BLOCKED_NAMES.has(lower) || BLOCKED_PREFIXES.some((prefix) => lower.startsWith(prefix))) {
    return `${name} belongs to Paperboat or the browser and can’t be changed.`
  }
  if (rule.op !== "remove" && /[\u0000-\u0008\u000a-\u001f\u007f]/.test(rule.value ?? "")) return "A value can’t contain line breaks or control characters."
  return undefined
}

const pathProblem = (value: string) =>
  !/^\/(?!\/)[^?#\\\s]*$/.test(value) || value.split("/").some((part) => part === "." || part === "..")
    ? "Start with a single /, with no spaces, ? or # and no . or .. parts."
    : undefined

function redirectProblem(rule: RouteRedirect) {
  const fromProblem = pathProblem(rule.from)
  if (fromProblem) return fromProblem
  const to = rule.to
  if (!/^\/(?!\/)[^\s\\]*$/.test(to) && !/^https:\/\/[^\s/@\\]+[^\s\\]*$/.test(to)) return "Send to a path like /new or an https:// address."
  const target = to.split(/[?#]/)[0]
  const prefix = rule.from.replace(/\/+$/, "")
  if (to.startsWith("/") && (prefix === "" || target === prefix || target.startsWith(`${prefix}/`))) {
    return "That sends the path to itself or below it, so it would redirect again."
  }
  return undefined
}

function HeaderRows({
  label,
  rules,
  onChange,
}: {
  label: string
  rules: RouteHeaderRule[]
  onChange: (next: RouteHeaderRule[]) => void
}) {
  const update = (index: number, patch: Partial<RouteHeaderRule>) =>
    onChange(rules.map((rule, i) => (i === index ? { ...rule, ...patch } : rule)))
  return (
    <Field className="gap-1.5">
      <FieldLabel className="flex items-baseline justify-between">
        {label}
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-label={`Add ${label.toLowerCase()}`}
          disabled={rules.length >= MAX_HEADERS}
          onClick={() => onChange([...rules, { op: "set", name: "", value: "" }])}
        >
          <PlusIcon />
          Add
        </Button>
      </FieldLabel>
      {rules.length === 0 && <p className="text-xs text-muted-foreground">No rules yet.</p>}
      {rules.map((rule, index) => {
        const problem = rule.name ? headerProblem(rule) : undefined
        return (
          <div key={index} className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <ToggleGroup
                value={[rule.op]}
                onValueChange={(value) => value[0] && update(index, { op: value[0] as RouteHeaderRule["op"] })}
                variant="outline"
                spacing={0}
              >
                {(["set", "add", "remove"] as const).map((op) => (
                  <ToggleGroupItem key={op} value={op} className="h-8 px-2 text-xs">
                    {op === "set" ? "Set" : op === "add" ? "Add" : "Remove"}
                  </ToggleGroupItem>
                ))}
              </ToggleGroup>
              <Input
                aria-label={`${label} name`}
                placeholder="X-Name"
                value={rule.name}
                onChange={(event) => update(index, { name: event.target.value })}
                aria-invalid={Boolean(problem)}
                className="min-w-0 flex-1 font-mono"
                autoComplete="off"
                spellCheck={false}
              />
              {rule.op !== "remove" && (
                <Input
                  aria-label={`${label} value`}
                  placeholder="value"
                  value={rule.value ?? ""}
                  onChange={(event) => update(index, { value: event.target.value })}
                  className="min-w-0 flex-1 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                />
              )}
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label="Remove rule"
                onClick={() => onChange(rules.filter((_, i) => i !== index))}
              >
                <TrashIcon />
              </Button>
            </div>
            {problem && <FieldError>{problem}</FieldError>}
          </div>
        )
      })}
    </Field>
  )
}

export function RouteRulesDialog({ tunnel, route, onClose }: { tunnel: Tunnel; route: TunnelRoute; onClose: () => void }) {
  const queryClient = useQueryClient()
  const current = route.rewrite_rules
  const [requestHeaders, setRequestHeaders] = useState<RouteHeaderRule[]>(current?.request_headers ?? [])
  const [responseHeaders, setResponseHeaders] = useState<RouteHeaderRule[]>(current?.response_headers ?? [])
  const [from, setFrom] = useState(current?.path_rewrite?.from ?? "")
  const [to, setTo] = useState(current?.path_rewrite?.to ?? "")
  const [redirects, setRedirects] = useState<RouteRedirect[]>(current?.redirects ?? [])

  const rewriteProblem =
    from || to ? (pathProblem(from) ?? (to.trim() ? pathProblem(to) : "Enter where the path goes.")) : undefined
  const redirectProblems = redirects.map(redirectProblem)
  const headerProblems = [...requestHeaders, ...responseHeaders].map((rule) => headerProblem(rule))
  const ready = !rewriteProblem && redirectProblems.every((p) => !p) && headerProblems.every((p) => !p)

  const save = useMutation({
    mutationFn: () => {
      const clean = (list: RouteHeaderRule[]) =>
        list.map((rule) =>
          rule.op === "remove" ? { op: rule.op, name: rule.name.trim() } : { op: rule.op, name: rule.name.trim(), value: rule.value ?? "" },
        )
      const rules: RouteRules = {}
      if (requestHeaders.length) rules.request_headers = clean(requestHeaders)
      if (responseHeaders.length) rules.response_headers = clean(responseHeaders)
      if (from.trim()) rules.path_rewrite = { from: from.trim(), to: to.trim() }
      if (redirects.length) rules.redirects = redirects.map((r) => ({ from: r.from.trim(), code: r.code, to: r.to.trim() }))
      return patchRoute(route, { rewrite_rules: rules })
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: tunnelKeys.routes(tunnel.id) })
      onClose()
    },
  })

  return (
    <Dialog open onOpenChange={(next) => !next && !save.isPending && onClose()}>
      <DialogContent className="max-h-[85vh] gap-4 overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Edit rules</DialogTitle>
        </DialogHeader>
        <form
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (ready) save.mutate()
          }}
        >
          <HeaderRows label="Request headers" rules={requestHeaders} onChange={setRequestHeaders} />
          <HeaderRows label="Response headers" rules={responseHeaders} onChange={setResponseHeaders} />
          <Field data-invalid={Boolean(rewriteProblem)} className="gap-1.5 border-t border-divider pt-4">
            <FieldLabel className="flex items-baseline justify-between">
              Path
              <span className="text-xs font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <div className="flex items-center gap-2">
              <Input
                aria-label="Path to replace"
                placeholder="/api"
                value={from}
                onChange={(event) => setFrom(event.target.value)}
                className="min-w-0 flex-1 font-mono"
                autoComplete="off"
                spellCheck={false}
              />
              <span className="text-muted-foreground">→</span>
              <Input
                aria-label="Path to use instead"
                placeholder="/v1"
                value={to}
                onChange={(event) => setTo(event.target.value)}
                className="min-w-0 flex-1 font-mono"
                autoComplete="off"
                spellCheck={false}
              />
            </div>
            {rewriteProblem ? (
              <FieldError>{rewriteProblem}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">Your app sees the new path. /api also covers /api/users.</p>
            )}
          </Field>
          <Field className="gap-1.5 border-t border-divider pt-4">
            <FieldLabel className="flex items-baseline justify-between">
              Redirects
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label="Add redirect"
                disabled={redirects.length >= MAX_REDIRECTS}
                onClick={() => setRedirects([...redirects, { from: "", code: 308, to: "" }])}
              >
                <PlusIcon />
                Add
              </Button>
            </FieldLabel>
            {redirects.length === 0 && <p className="text-xs text-muted-foreground">No redirects yet.</p>}
            {redirects.map((redirect, index) => {
              const problem = redirect.from || redirect.to ? redirectProblems[index] : undefined
              const update = (patch: Partial<RouteRedirect>) =>
                setRedirects(redirects.map((r, i) => (i === index ? { ...r, ...patch } : r)))
              return (
                <div key={index} className="flex flex-col gap-1">
                  <div className="flex items-center gap-2">
                    <Input
                      aria-label="Redirect from"
                      placeholder="/old"
                      value={redirect.from}
                      onChange={(event) => update({ from: event.target.value })}
                      className="min-w-0 flex-1 font-mono"
                      autoComplete="off"
                      spellCheck={false}
                    />
                    <ToggleGroup
                      value={[String(redirect.code)]}
                      onValueChange={(value) => value[0] && update({ code: Number(value[0]) as RouteRedirect["code"] })}
                      variant="outline"
                      spacing={0}
                    >
                      {[301, 302, 307, 308].map((code) => (
                        <ToggleGroupItem key={code} value={String(code)} className="h-8 px-2 text-xs">
                          {code}
                        </ToggleGroupItem>
                      ))}
                    </ToggleGroup>
                    <Input
                      aria-label="Redirect to"
                      placeholder="/new"
                      value={redirect.to}
                      onChange={(event) => update({ to: event.target.value })}
                      className="min-w-0 flex-1 font-mono"
                      autoComplete="off"
                      spellCheck={false}
                    />
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      aria-label="Remove redirect"
                      onClick={() => setRedirects(redirects.filter((_, i) => i !== index))}
                    >
                      <TrashIcon />
                    </Button>
                  </div>
                  {problem && <FieldError>{problem}</FieldError>}
                </div>
              )
            })}
          </Field>
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!ready || save.isPending}>
              {save.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
