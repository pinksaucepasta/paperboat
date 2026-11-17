"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState, useSyncExternalStore } from "react"
import { newToken, tokenId, type NewToken } from "@/lib/recipe-token"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { confirmation, revokeRecipe, rotateRecipe, type Impact, type Recipe } from "@/lib/recipes"
import { queryKeys } from "@/lib/query-keys"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { TokenOnce } from "@/components/recipes/token-once"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ErrorMessage } from "@/components/error-reference"

const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
const names = (impact: Impact) => impact.machines.map((machine) => machine.name).join(", ")

/**
 * The server refuses a rotate or revoke until it is sent the current code, and the refusal says what
 * would happen. Asked once on open (it changes nothing), then the person types the code to go on.
 * A server that does the action without a code is read as done.
 */
function useImpact(ask: (confirm: string) => Promise<unknown>, key: unknown[]) {
  return useQuery({
    queryKey: key,
    gcTime: 0,
    retry: false,
    queryFn: async () => {
      try {
        await ask("")
      } catch (error) {
        const found = confirmation(error)
        if (found) return found
        throw error
      }
      return null
    },
  })
}

export function RevokeRecipe({ workspace, recipe, onClose, onDone }: Props & { onDone: () => void }) {
  const queryClient = useQueryClient()
  const [typed, setTyped] = useState("")
  const impact = useImpact((confirm) => revokeRecipe(workspace, recipe.name, confirm), ["recipe-revoke", workspace, recipe.id])
  const code = impact.data?.confirm
  const revoke = useMutation({
    mutationFn: () => revokeRecipe(workspace, recipe.name, code ?? ""),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.recipes(workspace) })
      onDone()
    },
  })
  const found = impact.data?.impact
  const error = impact.error ?? revoke.error
  return (
    <AlertDialog open onOpenChange={(open) => !open && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke {recipe.name}?</AlertDialogTitle>
        </AlertDialogHeader>
        {found && (
          <ul className="flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground">
            <li>Its token stops working.</li>
            <li>
              {found.machines.length === 0
                ? "It has no machines to remove."
                : `${count(found.machines.length, "machine is", "machines are")} removed: ${names(found)}.`}
            </li>
            {found.vaults.length > 0 && (
              <li>
                {count(found.vaults.length, "vault gets", "vaults get")} a new key: {found.vaults.join(", ")}.
              </li>
            )}
          </ul>
        )}
        {code && (
          <Field className="gap-1.5">
            <FieldLabel htmlFor="revoke-code">
              Type <span className="font-mono">{code}</span> to confirm
            </FieldLabel>
            <Input id="revoke-code" value={typed} autoComplete="off" onChange={(event) => setTyped(event.target.value.trim())} />
          </Field>
        )}
        {error && (
          <p className="text-sm text-destructive">
            <ErrorMessage error={error} />
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={revoke.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={!code || typed !== code || revoke.isPending}
            onClick={(event) => {
              event.preventDefault()
              revoke.mutate()
            }}
          >
            {revoke.isPending ? "Revoking…" : "Revoke recipe"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

type Props = { workspace: string; recipe: Recipe; onClose: () => void }

/** Rotate: the new token is made here and sent with the code; it is shown once when the server has it. */
export function RotateRecipe({ workspace, recipe, onClose }: Props) {
  const vaults = recipe.recipe.env?.vaults ?? []
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  // A recipe that reads vaults has its vault keys wrapped to the new token here, so ENV must be unlocked first.
  if (vaults.length && !unlocked)
    return (
      <Dialog open onOpenChange={(open) => !open && onClose()}>
        <DialogContent className="gap-4 sm:max-w-lg">
          <DialogTitle>Rotate {recipe.name}</DialogTitle>
          <EnvUnlock title={`${recipe.name} reads ${count(vaults.length, "vault", "vaults")}. Unlock ENV to give the new token their keys.`} />
        </DialogContent>
      </Dialog>
    )
  return <RotateHere workspace={workspace} recipe={recipe} onClose={onClose} />
}

function RotateHere({ workspace, recipe, onClose }: Props) {
  const queryClient = useQueryClient()
  const [typed, setTyped] = useState("")
  const [made, setMade] = useState<NewToken>()
  const token = useQuery({
    queryKey: ["recipe-rotate-token", recipe.id],
    gcTime: 0,
    staleTime: Infinity,
    queryFn: () => newToken(tokenId(recipe.token)),
  })
  // Asking for the impact sends a throwaway token; the code does not depend on it.
  const impact = useImpact(
    async (confirm) => rotateRecipe(workspace, recipe.name, await newToken(tokenId(recipe.token)), confirm),
    ["recipe-rotate", workspace, recipe.id],
  )
  const code = impact.data?.confirm
  const rotate = useMutation({
    mutationFn: async () => {
      const next = token.data
      if (!next) throw new Error("The new token isn’t ready yet. Try again.")
      await rotateRecipe(workspace, recipe.name, next, code ?? "")
      return next
    },
    onSuccess: async (next) => {
      setMade(next)
      await queryClient.invalidateQueries({ queryKey: queryKeys.recipes(workspace) })
    },
  })
  const found = impact.data?.impact
  const error = impact.error ?? rotate.error
  if (made) {
    return (
      <Dialog open onOpenChange={(open) => !open && onClose()}>
        <DialogContent className="gap-4 sm:max-w-lg">
          <DialogTitle>{recipe.name} rotated</DialogTitle>
          <TokenOnce token={made.token} onDone={onClose} />
        </DialogContent>
      </Dialog>
    )
  }
  return (
    <AlertDialog open onOpenChange={(open) => !open && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Rotate {recipe.name}?</AlertDialogTitle>
        </AlertDialogHeader>
        {found && (
          <ul className="flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground">
            <li>It gets a new token. The old one stops working at once.</li>
            <li>
              {found.machines.length === 0
                ? "It has no machines."
                : `Its ${count(found.machines.length, "machine keeps", "machines keep")} running.`}
            </li>
            <li>Replace the old token wherever you saved it.</li>
          </ul>
        )}
        {code && (
          <Field className="gap-1.5">
            <FieldLabel htmlFor="rotate-code">
              Type <span className="font-mono">{code}</span> to confirm
            </FieldLabel>
            <Input id="rotate-code" value={typed} autoComplete="off" onChange={(event) => setTyped(event.target.value.trim())} />
          </Field>
        )}
        {error && (
          <p className="text-sm text-destructive">
            <ErrorMessage error={error} />
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={rotate.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={!code || typed !== code || !token.data || rotate.isPending}
            onClick={(event) => {
              event.preventDefault()
              rotate.mutate()
            }}
          >
            {rotate.isPending ? "Rotating…" : "Rotate token"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
