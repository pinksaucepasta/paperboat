"use client"

import { CircleNotchIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useRouter } from "next/navigation"
import { useEffect, useState } from "react"
import { ApiError, type Me } from "@/lib/api"
import { browserSessionQuery, guestApi, type BrowserSession } from "@/lib/terminal/queries"
import { Frame, TerminalRoom, TopBar } from "@/components/terminal/terminal-page"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/**
 * `/terminal/{id}`, the link a terminal is shared by. People with access through their account
 * go straight to the terminal. Anyone else signs in, or enters the join code and a name and joins
 * as a guest, with or without an account. A link sent with a code (`withCode`) asks for it first.
 */
export function TerminalJoin({ id, withCode }: { id: string; withCode: boolean }) {
  const router = useRouter()
  // Joined guests keep the session fresh here (they can't read the account-only endpoints).
  const access = useQuery({
    ...browserSessionQuery(id),
    refetchInterval: (query) => (query.state.data ? 4_000 : false),
  })
  const account = access.data?.access_method === "account"

  useEffect(() => {
    if (account) router.replace(`/terminals/${encodeURIComponent(id)}`)
  }, [account, id, router])

  // Who is signed in, if anyone; a 401 here is just "nobody", not a reason to send them to sign in.
  const me = useQuery({
    queryKey: ["terminal-join", "me"],
    queryFn: () => guestApi<Me>("me"),
    retry: false,
    enabled: access.isError,
  })
  const [askCode, setAskCode] = useState(withCode)

  if (access.data?.access_method === "code") return <TerminalRoom session={access.data.session} guest />
  if (access.isPending || account || me.isPending) {
    return (
      <Frame>
        <TopBar />
        <main className="flex flex-1 items-center justify-center">
          <CircleNotchIcon className="size-5 animate-spin text-muted-foreground" />
        </main>
      </Frame>
    )
  }
  const signedIn = me.data
  if (!askCode) return <NoAccess id={id} signedIn={signedIn} onCode={() => setAskCode(true)} />
  return <JoinForm id={id} signedIn={signedIn} />
}

const signInHref = (id: string) => `/auth/sign-in?return_to=${encodeURIComponent(`/terminal/${id}`)}`

/** A plain link: the terminal is shared with particular people, so signing in comes first. */
function NoAccess({ id, signedIn, onCode }: { id: string; signedIn?: Me; onCode: () => void }) {
  return (
    <Frame>
      <TopBar />
      <main className="flex flex-1 items-center justify-center p-6">
        <div className="flex w-full max-w-sm flex-col gap-5">
          <div className="flex flex-col gap-1.5">
            <h1 className="text-xl font-semibold">
              {signedIn ? "You don’t have access" : "Sign in to open this terminal"}
            </h1>
            <p className="text-sm text-muted-foreground">
              {signedIn ? (
                <>
                  You’re signed in as <span className="font-medium text-foreground">{signedIn.email}</span>. Ask the
                  person who shared it to add this email, or join with a code.
                </>
              ) : (
                "Use the account the terminal was shared with."
              )}
            </p>
          </div>
          {signedIn ? (
            <Button size="lg" onClick={onCode}>
              Join with a code
            </Button>
          ) : (
            <>
              <Button size="lg" render={<a href={signInHref(id)} />} nativeButton={false}>
                Sign in
              </Button>
              <p className="text-center text-sm text-muted-foreground">
                Have a join code?{" "}
                <button
                  type="button"
                  className="font-medium text-foreground underline-offset-4 hover:underline"
                  onClick={onCode}
                >
                  Enter it
                </button>
              </p>
            </>
          )}
        </div>
      </main>
    </Frame>
  )
}

function JoinForm({ id, signedIn }: { id: string; signedIn?: Me }) {
  const queryClient = useQueryClient()
  const [code, setCode] = useState("")
  const [name, setName] = useState(signedIn?.display_name ?? "")
  const join = useMutation({
    mutationFn: () =>
      guestApi<BrowserSession>(`terminal-sessions/${encodeURIComponent(id)}/code-join`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ code: code.trim(), display_name: name.trim() }),
      }),
    onSuccess: (joined) => queryClient.setQueryData(browserSessionQuery(id).queryKey, joined),
    onError: () => setCode(""),
  })
  const problem =
    join.error instanceof ApiError && (join.error.status === 429 || join.error.code === "terminal_guest_capacity")
      ? join.error.message
      : join.isError
        ? "That code didn’t work. It may have expired or been replaced; ask for a new one."
        : ""

  return (
    <Frame>
      <TopBar />
      <main className="flex flex-1 items-center justify-center p-6">
        <form
          className="flex w-full max-w-sm flex-col gap-5"
          onSubmit={(event) => {
            event.preventDefault()
            if (code.trim() && name.trim()) join.mutate()
          }}
        >
          <div className="flex flex-col gap-1.5">
            <h1 className="text-xl font-semibold">Join a terminal</h1>
            <p className="text-sm text-muted-foreground">
              {signedIn
                ? `Enter the code you were sent. You’ll join as a guest, since ${signedIn.email} isn’t on this terminal.`
                : "Enter the code you were sent. No account needed."}
            </p>
          </div>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="join-code">Code</FieldLabel>
            <Input
              id="join-code"
              autoFocus
              value={code}
              onChange={(event) => setCode(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              maxLength={128}
              disabled={join.isPending}
              className="font-mono tracking-widest"
            />
          </Field>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="join-name">Your name</FieldLabel>
            <Input
              id="join-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoComplete="name"
              maxLength={64}
              disabled={join.isPending}
            />
            <FieldDescription>Shown to the people in the terminal.</FieldDescription>
          </Field>
          {problem && (
            <p role="alert" className="text-sm text-destructive">
              {problem}
            </p>
          )}
          <Button type="submit" size="lg" disabled={join.isPending || !code.trim() || !name.trim()}>
            {join.isPending ? "Joining…" : "Join"}
          </Button>
          {!signedIn && (
            <p className="text-center text-sm text-muted-foreground">
              Was it shared with your email?{" "}
              <a className="font-medium text-foreground underline-offset-4 hover:underline" href={signInHref(id)}>
                Sign in instead
              </a>
            </p>
          )}
        </form>
      </main>
    </Frame>
  )
}
