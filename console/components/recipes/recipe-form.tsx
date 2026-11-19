"use client"

import { PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { parseTree, setItem, setMember, valueOf } from "@/lib/jsonc-edit"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

type Obj = Record<string, unknown>
const topKeys = ["$schema", "tags", "reach", "expires", "max_machines", "remove_after_offline", "machine_lifetime", "env", "config", "preview", "tunnel"]
const words = (text: string) => text.split(/[\s,]+/).filter(Boolean)
const asList = (v: unknown) => (Array.isArray(v) ? v.map(String).join(", ") : "")

/** The recipe file as the form can show it, or why it cannot: a wrong type stops the form, never rewrites the file. */
function read(text: string): { recipe: Obj; extra: string[] } | { problem: string } {
  const root = parseTree(text)
  if (!root || root.kind !== "object") return { problem: "The file is not valid yet." }
  const recipe = valueOf(text, root) as Obj
  const isList = (k: string) => recipe[k] === undefined || Array.isArray(recipe[k])
  const isObject = (k: string) => recipe[k] === undefined || (typeof recipe[k] === "object" && !Array.isArray(recipe[k]) && recipe[k] !== null)
  const bad = ["tags", "reach", "preview", "tunnel"].find((k) => !isList(k)) ?? ["env", "config"].find((k) => !isObject(k))
  if (bad) return { problem: `“${bad}” has a shape the form cannot show.` }
  for (const key of ["reach", "preview", "tunnel"]) {
    if ((recipe[key] as unknown[] | undefined)?.some((x) => typeof x !== "object" || x === null || Array.isArray(x)))
      return { problem: `An entry of “${key}” has a shape the form cannot show.` }
  }
  return { recipe, extra: Object.keys(recipe).filter((k) => !topKeys.includes(k)) }
}

/** A text box that writes to the file when you leave it or press Enter, so a half-typed list is not rewritten. */
function Box({ id, label, value, onCommit, placeholder, optional = true, type }: { id: string; label: string; value: string; onCommit: (value: string) => void; placeholder?: string; optional?: boolean; type?: string }) {
  return (
    <Field className="gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <FieldLabel htmlFor={id}>{label}</FieldLabel>
        {optional && <span className="text-xs text-muted-foreground">Optional</span>}
      </div>
      <Input
        key={value}
        id={id}
        type={type}
        defaultValue={value}
        placeholder={placeholder}
        autoComplete="off"
        spellCheck={false}
        onBlur={(event) => event.target.value !== value && onCommit(event.target.value)}
        onKeyDown={(event) => {
          if (event.key !== "Enter") return
          event.preventDefault()
          event.currentTarget.blur()
        }}
      />
    </Field>
  )
}

/**
 * A form over the recipe file. Every field edits the file's text in place (lib/jsonc-edit): comments and
 * keys the form does not know stay as they are, and the file view shows the same text.
 */
export function RecipeForm({ text, onChange, openFile }: { text: string; onChange: (text: string) => void; openFile: () => void }) {
  const parsed = read(text)
  if ("problem" in parsed) {
    return (
      <div className="flex flex-col items-start gap-3 surface rounded-lg p-4 text-sm" data-recipe-form="unsupported">
        <p>{parsed.problem} Edit this file as text.</p>
        <Button type="button" variant="outline" onClick={openFile}>
          Open the file
        </Button>
      </div>
    )
  }
  const { recipe, extra } = parsed
  const env = (recipe.env ?? {}) as Obj
  const config = (recipe.config ?? {}) as Obj
  const set = (key: string, value: unknown) => onChange(setMember(text, key, value))
  const text0 = (key: string, value: string) => set(key, value.trim() || undefined)
  const number = (key: string, value: string) => set(key, value.trim() === "" ? undefined : Number(value))
  const section = (key: "env" | "config", current: Obj, patch: Obj) => {
    const next = { ...current, ...patch }
    for (const k of Object.keys(next)) if (next[k] === undefined) delete next[k]
    set(key, Object.keys(next).length ? next : undefined)
  }
  const entries = (key: "preview" | "tunnel" | "reach") => ((recipe[key] as Obj[] | undefined) ?? [])
  const change = (key: "preview" | "tunnel" | "reach", index: number, patch: Obj) => {
    const next = { ...entries(key)[index], ...patch }
    for (const k of Object.keys(next)) if (next[k] === undefined || next[k] === false) delete next[k]
    onChange(setItem(text, key, index, next))
  }

  return (
    <div className="flex flex-col gap-6" data-recipe-form="fields">
      <div className="grid gap-4 sm:grid-cols-2">
        <Box id="f-tags" label="Tags" value={asList(recipe.tags)} placeholder="ci, linux" onCommit={(v) => set("tags", words(v).length ? words(v) : undefined)} />
        <Box id="f-expires" label="Recipe expires after" value={String(recipe.expires ?? "")} placeholder="90d" onCommit={(v) => text0("expires", v)} />
        <Box id="f-max" label="Most machines" type="number" value={String(recipe.max_machines ?? "")} placeholder="20" onCommit={(v) => number("max_machines", v)} />
        <Box id="f-offline" label="Remove after offline" value={String(recipe.remove_after_offline ?? "")} placeholder="30m" onCommit={(v) => text0("remove_after_offline", v)} />
        <Box id="f-life" label="Machine lifetime" value={String(recipe.machine_lifetime ?? "")} placeholder="8h" onCommit={(v) => text0("machine_lifetime", v)} />
      </div>

      <section className="flex flex-col gap-3">
        <SectionHeading title="Secrets and config" />
        <div className="grid gap-4 sm:grid-cols-2">
          <Box id="f-vaults" label="ENV vaults" value={asList(env.vaults)} placeholder="canary-env" onCommit={(v) => section("env", env, { vaults: words(v) })} />
          <Box id="f-repo" label="Config repository" value={String(config.repository ?? "")} placeholder="dotfiles" onCommit={(v) => section("config", config, { repository: v.trim() || undefined })} />
        </div>
        {recipe.env !== undefined && (
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={env.shell !== false} onCheckedChange={(on) => section("env", env, { shell: on ? undefined : false })} />
            Also set the values in terminals and commands
          </label>
        )}
      </section>

      <Entries
        title="Previews"
        add="Add preview"
        items={entries("preview")}
        onAdd={() => onChange(setItem(text, "preview", entries("preview").length, { port: 3000, ttl: "1h" }))}
        onRemove={(i) => onChange(setItem(text, "preview", i, undefined))}
        render={(item, i) => (
          <>
            <Box id={`p-port-${i}`} label="Port" type="number" optional={false} value={String(item.port ?? "")} onCommit={(v) => change("preview", i, { port: v ? Number(v) : undefined })} />
            <Box id={`p-ttl-${i}`} label="Open for" value={String(item.ttl ?? "")} placeholder="1h" onCommit={(v) => change("preview", i, { ttl: v.trim() || undefined })} />
            <Flags item={item} onChange={(patch) => change("preview", i, patch)} />
          </>
        )}
      />
      <Entries
        title="Tunnels"
        add="Add tunnel"
        items={entries("tunnel")}
        onAdd={() => onChange(setItem(text, "tunnel", entries("tunnel").length, { port: 8080 }))}
        onRemove={(i) => onChange(setItem(text, "tunnel", i, undefined))}
        render={(item, i) => (
          <>
            <Box id={`t-name-${i}`} label="Name" value={String(item.name ?? "")} onCommit={(v) => change("tunnel", i, { name: v.trim() || undefined })} />
            <Box id={`t-port-${i}`} label="Port" type="number" value={String(item.port ?? "")} onCommit={(v) => change("tunnel", i, { port: v ? Number(v) : undefined })} />
            <Box id={`t-from-${i}`} label="Or address" value={String(item.from ?? "")} placeholder="http://localhost:8080" onCommit={(v) => change("tunnel", i, { from: v.trim() || undefined })} />
            <Flags item={item} onChange={(patch) => change("tunnel", i, patch)} />
          </>
        )}
      />
      <Entries
        title="What it may reach"
        add="Add reach"
        items={entries("reach")}
        onAdd={() => onChange(setItem(text, "reach", entries("reach").length, { dst: ["tag:ci"], allow: ["exec"] }))}
        onRemove={(i) => onChange(setItem(text, "reach", i, undefined))}
        render={(item, i) => (
          <>
            <Box id={`r-dst-${i}`} label="Machines" optional={false} value={asList(item.dst)} placeholder="tag:ci" onCommit={(v) => change("reach", i, { dst: words(v) })} />
            <Box id={`r-allow-${i}`} label="Actions" optional={false} value={asList(item.allow)} placeholder="exec, files" onCommit={(v) => change("reach", i, { allow: words(v) })} />
          </>
        )}
      />
      {extra.length > 0 && (
        <p className="text-xs text-muted-foreground" data-recipe-form="extra">
          Also in the file, kept as written: {extra.join(", ")}.
        </p>
      )}
    </div>
  )
}

function Flags({ item, onChange }: { item: Obj; onChange: (patch: Obj) => void }) {
  return (
    <div className="flex items-end gap-4 pb-2 text-sm">
      {(["private", "team"] as const).map((flag) => (
        <label key={flag} className="flex items-center gap-2">
          <Checkbox checked={item[flag] === true} onCheckedChange={(on) => onChange({ [flag]: on ? true : undefined })} />
          {flag === "private" ? "Signed-in only" : "Team only"}
        </label>
      ))}
    </div>
  )
}

function Entries({ title, add, items, onAdd, onRemove, render }: { title: string; add: string; items: Obj[]; onAdd: () => void; onRemove: (index: number) => void; render: (item: Obj, index: number) => React.ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <SectionHeading
        title={title}
        action={
          <Button type="button" variant="outline" onClick={onAdd}>
            <PlusIcon data-icon="inline-start" />
            {add}
          </Button>
        }
      />
      {items.map((item, index) => (
        <div key={index} className="flex items-start gap-2 surface rounded-lg p-3">
          <div className="grid min-w-0 flex-1 gap-3 sm:grid-cols-2">{render(item, index)}</div>
          <Button type="button" variant="ghost" size="icon" aria-label={`Remove ${title.toLowerCase()} ${index + 1}`} onClick={() => onRemove(index)}>
            <TrashIcon />
          </Button>
        </div>
      ))}
    </section>
  )
}

