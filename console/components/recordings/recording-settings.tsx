"use client"

import { TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState, useSyncExternalStore } from "react"
import { ApiError, useApiQuery } from "@/lib/api"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { policyQuery, savePolicy, validatePolicy, type Policy } from "@/lib/policy"
import {
  createRecordingKey,
  editRecordingRules,
  putManualStart,
  recordingKeyQuery,
  recordingMessage,
  recordingRules,
  keyHeld,
  resetRecordingKey,
  rotateRecordingKey,
  subjectLabel,
  type RecordingAction,
  type RecordingKeyState,
  type RecordingViewer,
} from "@/lib/recordings"
import type { Team } from "@/lib/teams"
import { ChangesDialog } from "@/components/access/changes-dialog"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { ConfirmAction } from "@/components/list-toolbar"
import { RecordingAccess } from "@/components/recordings/recording-access"
import { Row, Section, SettingRow } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { ErrorMessage } from "@/components/error-reference"
import { useShellState } from "@/lib/use-shell-state"

/**
 * Team → Recordings (the personal workspace's key is set up in PersonalRecordingSetup): the team recording key, who may start recordings by hand, and the policy rules that record
 * sessions or allow replay. Owners and admins change them; auditors see them.
 */
export function RecordingSettings({
  team,
  emails,
  admin,
  owner,
}: {
  team: Team
  emails: Map<string, string>
  admin: boolean
  owner: boolean
}) {
  const key = useApiQuery(recordingKeyQuery(team.team_id))
  const policy = useApiQuery(policyQuery(team.team_id))
  if (key.error instanceof ApiError && key.error.code === "team_plan_required")
    return (
      <p className="rounded-lg bg-surface px-4 py-3 text-sm text-muted-foreground" data-recordings-upgrade>
        This team needs the Teams plan to record.{" "}
        <Link href="/billing" className="text-foreground underline underline-offset-4">
          Upgrade
        </Link>
      </p>
    )
  if (key.isError)
    return (
      <p className="text-sm text-muted-foreground">
        {<ErrorMessage error={key.error} message={recordingMessage(key.error)} />}
      </p>
    )
  return (
    <div className="flex flex-col gap-10" data-recording-settings>
      <RecordingAccess teamId={team.team_id} />
      <KeySection teamId={team.team_id} state={key.data} emails={emails} admin={admin} owner={owner} />
      <ManualStartSection teamId={team.team_id} state={key.data} admin={admin} />
      <RulesSection policy={policy.data} admin={admin} />
    </div>
  )
}

const viewerStatus = (viewer: RecordingViewer, hasKey: boolean) =>
  !viewer.sharing_public_key
    ? "No ENV vault yet"
    : !hasKey
      ? "Gets a copy at setup"
      : viewer.has_grant
        ? "Can replay"
        : "Waiting for a copy"

function KeySection({
  teamId,
  state,
  emails,
  admin,
  owner,
}: {
  teamId: string
  state: RecordingKeyState | undefined
  emails: Map<string, string>
  admin: boolean
  owner: boolean
}) {
  const queryClient = useQueryClient()
  const sealable = (state?.viewers ?? []).filter((viewer): viewer is Required<RecordingViewer> =>
    Boolean(viewer.sharing_public_key),
  )
  const create = useMutation({
    mutationFn: () => createRecordingKey(teamId, sealable),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(teamId).queryKey }),
  })
  const [replacing, setReplacing] = useState(false)
  const [resetting, setResetting] = useState(false)
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const [asked, setAsked] = useState(false)
  const setUp = admin && state && !state.key && (
    <Button size="lg" disabled={create.isPending || sealable.length === 0} onClick={() => create.mutate()}>
      {create.isPending ? "Setting up…" : "Set up"}
    </Button>
  )
  // Replacing needs this person's own copy of the current key: it signs the new one.
  const replaceable = admin && state?.key && state.my_key?.generation === state.key.generation
  // Resetting is for a key nobody holds any more, and only the team owner's ENV key can sign it.
  const resettable = owner && state?.key?.reset_public_key && !keyHeld(state)
  const reset = resettable && (
    <Button
      variant="outline"
      size="lg"
      onClick={() => (unlocked ? setResetting(true) : setAsked(true))}
      data-recording-key-reset
    >
      Reset key
    </Button>
  )
  const replace = replaceable && (
    <Button
      variant="outline"
      size="lg"
      onClick={() => (unlocked ? setReplacing(true) : setAsked(true))}
      data-recording-key-replace
    >
      Replace key
    </Button>
  )
  return (
    <Section title="Recording key" action={setUp || replace || reset || undefined}>
      <Row label="Status">
        {!state ? (
          <Skeleton className="my-0.5 h-4 w-24" />
        ) : state.key ? (
          <span data-recording-key="ready" data-generation={state.key.generation}>
            Ready <span className="text-muted-foreground">(generation {state.key.generation})</span>
          </span>
        ) : (
          <span data-recording-key="missing">Not set up</span>
        )}
      </Row>
      {state && !state.key && sealable.length === 0 && (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          Add a replay rule first. The key is encrypted for each person who can replay.
        </p>
      )}
      {asked && !unlocked && (
        <EnvUnlock title="Unlock your vault to change the recording key. It signs the new key in this browser." />
      )}
      {replacing && state && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setReplacing(false)}
          title="Replace the recording key?"
          description="New recordings are sealed to a new key, made and signed in this browser. Everyone who can replay keeps access and older recordings stay readable. Machines accept the new key only because your current key signed it."
          action="Replace key"
          pending="Replacing…"
          confirmName={teamId}
          mutation={{
            mutationFn: () => rotateRecordingKey(teamId, state),
            onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(teamId).queryKey }),
          }}
        />
      )}
      {resetting && state && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setResetting(false)}
          title="Reset the recording key?"
          description="Nobody holds the current recording key, so nobody can replace it. This makes a new key, signed in this browser with your ENV key as the team owner. Recordings made before can never be opened again, by anyone, you included. Everyone who can replay gets a copy of the new key. Machines accept it only because your signature verifies under the key they pinned for this team."
          action="Reset key, discard old recordings"
          pending="Resetting…"
          confirmName={teamId}
          mutation={{
            mutationFn: () => resetRecordingKey(teamId, state),
            onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(teamId).queryKey }),
          }}
        />
      )}
      {create.isError && (
        <p className="border-b border-divider py-3 text-sm text-destructive">
          {<ErrorMessage error={create.error} message={recordingMessage(create.error)} />}
        </p>
      )}
      {state?.viewers.map((viewer) => (
        <Row key={viewer.account_id} label={emails.get(viewer.account_id) ?? viewer.account_id}>
          <span className="text-muted-foreground">{viewerStatus(viewer, Boolean(state.key))}</span>
        </Row>
      ))}
    </Section>
  )
}

function ManualStartSection({
  teamId,
  state,
  admin,
}: {
  teamId: string
  state: RecordingKeyState | undefined
  admin: boolean
}) {
  const queryClient = useQueryClient()
  const change = useMutation({
    mutationFn: (adminsOnly: boolean) => putManualStart(teamId, adminsOnly ? "admins" : "anyone"),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: recordingKeyQuery(teamId).queryKey }),
  })
  const adminsOnly = (change.isPending ? change.variables : state?.manual_start === "admins") ?? false
  return (
    <Section title="Starting by hand">
      <SettingRow
        label="Only admins start recordings"
        description="Otherwise anyone in a terminal can start recording it."
        htmlFor="recording-admins-only"
      >
        <Switch
          id="recording-admins-only"
          checked={adminsOnly}
          disabled={!admin || !state || change.isPending}
          onCheckedChange={(checked) => change.mutate(checked)}
        />
      </SettingRow>
      {change.isError && (
        <p className="py-3 text-sm text-destructive">
          {<ErrorMessage error={change.error} message={recordingMessage(change.error)} />}
        </p>
      )}
    </Section>
  )
}

const actionTitles: Record<RecordingAction, string> = { record: "Recorded", recording_view: "Can replay" }

function RulesSection({ policy, admin }: { policy: Policy | undefined; admin: boolean }) {
  const queryClient = useQueryClient()
  const [adding, setAdding] = useShellState("recordings/recording-settings.tsx:adding", false)
  const [saving, setSaving] = useState<{ title: string; text: string } | null>(null)
  const rules = policy ? recordingRules(policy.policy) : []
  const editable = admin && policy?.can_edit === "all"
  // Rewriting a file with comments would drop them; such files are edited on Access.
  const rewritable = policy
    ? editRecordingRules(policy.policy, { add: { action: "record", src: "*", dst: "machine:*" } }) !== undefined
    : false
  return (
    <Section
      title="Rules"
      info={{
        title: "Rules",
        body: "Terminal sessions of the people a rule names are recorded on its machines. SSH and run commands are not recorded.",
      }}
      action={
        editable && rewritable ? (
          <Button size="lg" variant="outline" onClick={() => setAdding(true)}>
            Add rule
          </Button>
        ) : undefined
      }
    >
      {!policy ? (
        <Row label="Recorded">
          <Skeleton className="my-0.5 h-4 w-40" />
        </Row>
      ) : rules.length === 0 ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">No recording or replay rules.</p>
      ) : (
        rules.map((rule) => (
          <div
            key={`${rule.index}-${rule.action}`}
            className="flex min-h-12 items-center gap-4 border-b border-divider py-2 text-sm"
            data-recording-rule={rule.action}
          >
            <span className="w-24 shrink-0 text-muted-foreground">{actionTitles[rule.action]}</span>
            <span className="min-w-0 flex-1 truncate">
              {rule.src.map(subjectLabel).join(", ")} <span className="text-muted-foreground">on</span>{" "}
              {rule.dst.map(subjectLabel).join(", ")}
            </span>
            {editable && rewritable && (
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Remove rule"
                onClick={() =>
                  setSaving({ title: "Remove rule", text: editRecordingRules(policy.policy, { remove: rule })! })
                }
              >
                <TrashIcon />
              </Button>
            )}
          </div>
        ))
      )}
      {editable && policy && !rewritable && (
        <p className="py-3 text-sm text-muted-foreground">
          The policy file has comments. Change recording rules in the policy file.
        </p>
      )}
      {adding && policy && (
        <AddRuleDialog
          policy={policy}
          onClose={() => setAdding(false)}
          onReady={(text) => {
            setAdding(false)
            setSaving({ title: "Add rule", text })
          }}
        />
      )}
      {saving && policy && (
        <ChangesDialog
          workspace={policy.workspace}
          title={saving.title}
          verb="Save"
          review={(signal) => validatePolicy(policy.workspace, saving.text, signal)}
          run={(fields) =>
            savePolicy(policy.workspace, { policy: saving.text, expected_version: policy.version, ...fields })
          }
          onClose={() => setSaving(null)}
          onDone={() => {
            setSaving(null)
            // A replay rule adds or removes viewers, which the key section seals for.
            void queryClient.invalidateQueries({ queryKey: recordingKeyQuery(policy.workspace).queryKey })
          }}
        />
      )}
    </Section>
  )
}

type Who = "*" | "role:admin" | "group" | "user"
type Where = "machine:*" | "tag" | "machine"

/** Who and where for one rule; the policy check that follows lists who gains what before it saves. */
function AddRuleDialog({
  policy,
  onClose,
  onReady,
}: {
  policy: Policy
  onClose: () => void
  onReady: (text: string) => void
}) {
  const [action, setAction] = useShellState<RecordingAction>("recordings/recording-settings.tsx:action", "record")
  const [who, setWho] = useShellState<Who>("recordings/recording-settings.tsx:who", "*")
  const [whoName, setWhoName] = useShellState("recordings/recording-settings.tsx:whoName", "")
  const [where, setWhere] = useShellState<Where>("recordings/recording-settings.tsx:where", "tag")
  const [whereName, setWhereName] = useShellState("recordings/recording-settings.tsx:whereName", "")
  const src =
    who === "group" || who === "user" ? (whoName ? `${who}:${whoName.replace(/^(group|user):/, "")}` : "") : who
  const dst =
    where === "tag" || where === "machine"
      ? whereName
        ? `${where}:${whereName.replace(/^(tag|machine):/, "")}`
        : ""
      : where
  const options = (list: string[], prefix: string) => list.map((name) => name.replace(prefix, ""))
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add rule</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            const text = src && dst ? editRecordingRules(policy.policy, { add: { action, src, dst } }) : undefined
            if (text) onReady(text)
          }}
        >
          <ToggleGroup
            value={[action]}
            onValueChange={(next) => next[0] && setAction(next[0] as RecordingAction)}
            variant="outline"
            spacing={0}
          >
            <ToggleGroupItem value="record">Record sessions</ToggleGroupItem>
            <ToggleGroupItem value="recording_view">Allow replay</ToggleGroupItem>
          </ToggleGroup>
          <div className="flex flex-col gap-4 border-t border-divider pt-4">
            <Field className="gap-1.5">
              <FieldLabel>Who</FieldLabel>
              <ToggleGroup
                value={[who]}
                onValueChange={(next) => next[0] && setWho(next[0] as Who)}
                variant="outline"
                spacing={0}
              >
                <ToggleGroupItem value="*">Everyone</ToggleGroupItem>
                <ToggleGroupItem value="role:admin">Admins</ToggleGroupItem>
                <ToggleGroupItem value="group">Group</ToggleGroupItem>
                <ToggleGroupItem value="user">Member</ToggleGroupItem>
              </ToggleGroup>
              {(who === "group" || who === "user") && (
                <NameInput
                  label={who === "group" ? "Group" : "Email"}
                  value={whoName}
                  onChange={setWhoName}
                  options={
                    who === "group" ? options(policy.names.groups, "group:") : options(policy.names.users, "user:")
                  }
                />
              )}
            </Field>
            <Field className="gap-1.5">
              <FieldLabel>On</FieldLabel>
              <ToggleGroup
                value={[where]}
                onValueChange={(next) => next[0] && setWhere(next[0] as Where)}
                variant="outline"
                spacing={0}
              >
                <ToggleGroupItem value="tag">Tagged machines</ToggleGroupItem>
                <ToggleGroupItem value="machine">One machine</ToggleGroupItem>
                <ToggleGroupItem value="machine:*">All machines</ToggleGroupItem>
              </ToggleGroup>
              {(where === "tag" || where === "machine") && (
                <NameInput
                  label={where === "tag" ? "Tag" : "Machine"}
                  value={whereName}
                  onChange={setWhereName}
                  options={
                    where === "tag" ? options(policy.names.tags, "tag:") : options(policy.names.machines, "machine:")
                  }
                />
              )}
            </Field>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" size="lg" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!src || !dst}>
              Review
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function NameInput({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  options: string[]
}) {
  const list = `recording-${label.toLowerCase()}-names`
  return (
    <>
      <Input
        aria-label={label}
        placeholder={label}
        list={list}
        value={value}
        onChange={(event) => onChange(event.target.value.trim())}
        autoComplete="off"
        spellCheck={false}
      />
      <datalist id={list}>
        {options.map((name) => (
          <option key={name} value={name} />
        ))}
      </datalist>
    </>
  )
}
