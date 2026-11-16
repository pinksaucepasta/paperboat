"use client"

import { UploadSimpleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { useRef, useState, useSyncExternalStore } from "react"
import { toast } from "sonner"
import { ApiError, useWorkspace } from "@/lib/api"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { newToken, type NewToken } from "@/lib/recipe-token"
import {
  createRecipe,
  fileProblems,
  formatOf,
  nameFromFile,
  nameHint,
  updateRecipe,
  validName,
  type FileProblem,
  type Recipe,
} from "@/lib/recipes"
import { queryKeys } from "@/lib/query-keys"
import { createNamedVault, unknownVault, VaultLockedError } from "@/lib/recipe-vaults"
import { sections } from "@/lib/sections"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { TokenOnce } from "@/components/recipes/token-once"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"

const maxBytes = 64 << 10
const listPath = sections.recipes.href

/** Where the cursor goes for a problem: the start of its line. */
const lineStart = (text: string, line: number) =>
  text
    .split("\n")
    .slice(0, Math.max(line - 1, 0))
    .reduce((offset, row) => offset + row.length + 1, 0)

/**
 * New recipe, or the file of an existing one. The server reads the file (the same code as the CLI), so
 * its line-and-column errors show here as they are; the token is made in this browser and shown once.
 */
export function RecipeEditor({ recipe, start }: { recipe?: Recipe; start?: { name: string; source: string } }) {
  const workspace = useWorkspace()
  const router = useRouter()
  const queryClient = useQueryClient()
  const area = useRef<HTMLTextAreaElement>(null)
  const upload = useRef<HTMLInputElement>(null)
  // The token is made on the first try and kept, so a corrected file is sent with the same one.
  const token = useRef<NewToken>(undefined)
  const [name, setName] = useState(recipe?.name ?? start?.name ?? "")
  const [source, setSource] = useState(recipe?.source ?? start?.source ?? "")
  const [fileName, setFileName] = useState<string>()
  const [problem, setProblem] = useState<string>()
  const [created, setCreated] = useState<string>()
  const format = recipe?.format ?? formatOf(source, fileName)
  const save = useMutation({
    mutationFn: async () => {
      if (!workspace) throw new Error("Choose a workspace first.")
      if (recipe) return { updated: await updateRecipe(workspace.id, recipe, { source, format }) }
      token.current ??= await newToken()
      await createRecipe(workspace.id, { name, source, format }, token.current)
      return { token: token.current.token }
    },
    onSuccess: async (result) => {
      if (!workspace) return
      await queryClient.invalidateQueries({ queryKey: queryKeys.recipes(workspace.id) })
      if ("token" in result) return setCreated(result.token)
      const { recipe: now, online_machines: online } = result.updated
      toast.success(`${now.name} is at revision ${now.revision}`, {
        description: online
          ? `${online} online ${online === 1 ? "machine applies" : "machines apply"} it now.`
          : undefined,
      })
      router.push(`${listPath}/${encodeURIComponent(now.name)}`)
    },
  })
  // A recipe that names a vault the workspace lacks can make it here (the vault engine wraps its key).
  const makeVault = useMutation({
    mutationFn: async (vault: string) => {
      if (!workspace) throw new Error("Choose a workspace first.")
      await createNamedVault(workspace.id, vault, workspace.kind === "team" ? workspace.id : undefined)
    },
    onSuccess: () => {
      save.reset()
      save.mutate()
    },
  })
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const failed = makeVault.error ?? save.error
  const found = fileProblems(failed)
  const taken = failed instanceof ApiError && failed.code === "recipe_name_taken"
  const locked = failed instanceof VaultLockedError && !unlocked
  const missing = save.error ? unknownVault(save.error) : undefined
  const nameError = taken ? failed.message : name && !validName(name) ? nameHint : undefined
  const other =
    failed && !found.length && !taken && !problem && !(failed instanceof VaultLockedError) ? failed.message : undefined

  if (created) {
    return (
      <div className="flex max-w-xl flex-col gap-4">
        <TokenOnce token={created} onDone={() => router.push(`${listPath}/${encodeURIComponent(name)}`)} />
      </div>
    )
  }

  function read(file: File) {
    if (file.size > maxBytes) return setProblem("This file is over 64 KB. A recipe is a small file.")
    void file.text().then((text) => {
      setProblem(undefined)
      save.reset()
      setSource(text)
      setFileName(file.name)
      if (!recipe && !name) setName(nameFromFile(file.name))
    })
  }

  function jump(item: FileProblem) {
    const target = area.current
    if (!target || item.line < 1) return
    const at = lineStart(source, item.line) + Math.max(item.column - 1, 0)
    target.focus()
    target.setSelectionRange(at, at)
  }

  return (
    <div className="flex flex-col gap-4">
      <form
        className="flex flex-col gap-4"
        onSubmit={(event) => {
          event.preventDefault()
          setProblem(undefined)
          save.mutate()
        }}
      >
        {!recipe && (
          <Field className="max-w-md gap-1.5">
            <FieldLabel htmlFor="recipe-name">Name</FieldLabel>
            <Input
              id="recipe-name"
              value={name}
              autoComplete="off"
              spellCheck={false}
              placeholder="ci"
              aria-invalid={Boolean(nameError)}
              onChange={(event) => setName(event.target.value.toLowerCase())}
            />
            {nameError ? (
              <FieldError>{nameError}</FieldError>
            ) : (
              <span className="text-xs text-muted-foreground">{nameHint}</span>
            )}
          </Field>
        )}
        <Field className="gap-1.5">
          <div className="flex items-center justify-between gap-2">
            <FieldLabel htmlFor="recipe-file">File</FieldLabel>
            <Button type="button" variant="outline" onClick={() => upload.current?.click()}>
              <UploadSimpleIcon data-icon="inline-start" />
              Upload
            </Button>
            <input
              ref={upload}
              type="file"
              accept=".toml,.json,text/plain,application/json"
              className="hidden"
              aria-label="Upload a recipe file"
              onChange={(event) => {
                const file = event.target.files?.[0]
                event.target.value = ""
                if (file) read(file)
              }}
            />
          </div>
          <Textarea
            id="recipe-file"
            ref={area}
            value={source}
            rows={18}
            spellCheck={false}
            autoCapitalize="off"
            placeholder={'tags = ["ci"]\nmax_machines = 20\n'}
            aria-invalid={found.length > 0}
            className="min-h-72 font-mono text-xs leading-relaxed whitespace-pre"
            onChange={(event) => setSource(event.target.value)}
          />
        </Field>
        {found.length > 0 && (
          <ul className="flex flex-col gap-1 text-sm text-destructive" data-recipe-errors>
            {found.map((item, index) => (
              <li key={index}>
                <button
                  type="button"
                  className="text-left underline-offset-2 hover:underline"
                  onClick={() => jump(item)}
                >
                  {item.line > 0 ? `${item.line}:${item.column} ` : ""}
                  {item.message}
                </button>
              </li>
            ))}
          </ul>
        )}
        {(problem ?? other) && <p className="text-sm text-destructive">{problem ?? other}</p>}
        {failed instanceof VaultLockedError && unlocked && (
          <p className="text-sm text-muted-foreground">Unlocked. Save again.</p>
        )}
        {missing && (
          <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
            <span>The workspace has no vault named {missing}.</span>
            <Button
              type="button"
              variant="outline"
              disabled={makeVault.isPending}
              onClick={() => makeVault.mutate(missing)}
            >
              {makeVault.isPending ? "Creating…" : `Create vault ${missing}`}
            </Button>
          </div>
        )}
        <div className="flex gap-2">
          <Button
            type="button"
            variant="outline"
            size="lg"
            disabled={save.isPending}
            render={<Link href={recipe ? `${listPath}/${recipe.name}` : listPath} />}
          >
            Cancel
          </Button>
          <Button
            size="lg"
            type="submit"
            disabled={
              save.isPending || !workspace || !source.trim() || (recipe ? source === recipe.source : !validName(name))
            }
          >
            {save.isPending ? "Saving…" : recipe ? "Save changes" : "Create recipe"}
          </Button>
        </div>
      </form>
      {/* Outside the form: the card is a form of its own, and a nested one would submit this one (password in the URL). */}
      {locked && <EnvUnlock title="This recipe reads vaults. Unlock ENV to give it their keys, then save again." />}
    </div>
  )
}
