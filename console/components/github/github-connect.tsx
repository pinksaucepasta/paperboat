"use client"

import { CheckCircleIcon, CircleNotchIcon, GithubLogoIcon, XCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useSearchParams } from "next/navigation"
import { useEffect, useRef, useSyncExternalStore, type ReactNode } from "react"
import { ApiError } from "@/lib/api"
import {
  approveLink,
  cancelLink,
  githubCallbackQuery,
  githubErrors,
  githubStatusQuery,
  reviewLink,
  useGitHubConnect,
} from "@/lib/github"
import { useTimeFormat } from "@/lib/use-time-format"
import { BrandScreen, Heading, Outcome, brandPrimary, brandSecondary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"
import { ErrorMessage } from "@/components/error-reference"

/** The link's secret travels in the fragment (never sent to a server or a link preview). */
function useFragment(): string {
  return useSyncExternalStore(
    (notify) => {
      window.addEventListener("hashchange", notify)
      return () => window.removeEventListener("hashchange", notify)
    },
    () => window.location.hash.slice(1),
    () => "",
  )
}

const startAgain = "Run the pb command again to get a new link."

/**
 * The page `pb` opens to connect GitHub (/github/connect?link=…#token). Reading the request uses
 * nothing up, so a reload or a link preview can't break it; Approve sends you to GitHub, which
 * returns to /github/done.
 */
export function GitHubConnect() {
  const link = useSearchParams().get("link") ?? ""
  const token = useFragment()
  return <BrandScreen>{link && token ? <Review link={link} token={token} /> : <Missing />}</BrandScreen>
}

function Missing() {
  return (
    <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="This link is incomplete">
      Open the whole link your terminal printed. {startAgain}
    </Outcome>
  )
}

function Review({ link, token }: { link: string; token: string }) {
  const time = useTimeFormat()
  const review = useQuery({
    queryKey: ["github-link", link],
    queryFn: ({ signal }) => reviewLink(link, token, signal),
    retry: false,
  })
  const approve = useMutation({
    mutationFn: () => approveLink(link, token),
    onSuccess: ({ authorization_url }) => window.location.assign(authorization_url),
  })
  const cancel = useMutation({ mutationFn: () => cancelLink(link, token) })

  if (review.isPending) return <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
  if (review.isError) return <Problem error={review.error} />
  if (cancel.isSuccess) {
    return (
      <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="GitHub not connected">
        You cancelled it. {startAgain}
      </Outcome>
    )
  }
  if (approve.isSuccess) {
    return (
      <Outcome icon={<CircleNotchIcon className="size-8 animate-spin text-white" />} title="Opening GitHub…">
        GitHub asks you to authorize access, then brings you back here.
      </Outcome>
    )
  }

  const data = review.data
  const failed = approve.error ?? cancel.error
  return (
    <div className="flex flex-col gap-6">
      <GithubLogoIcon className="size-9 text-white" weight="fill" />
      <Heading
        title="Connect GitHub"
        text={
          <>
            For <span className="font-medium text-white">{data.account_email}</span>, requested by{" "}
            <code className="font-mono">pb</code> on <span className="font-medium text-white">{data.machine_name}</span>{" "}
            {time.relative(data.requested_at)}.
          </>
        }
      />
      <p className="text-sm text-white/75">Approve only if you just started this from your terminal.</p>
      {failed && (
        <p className="text-sm text-white">
          {
            <ErrorMessage
              error={failed}
              message={failed instanceof ApiError ? (githubErrors[failed.code] ?? failed.message) : failed.message}
            />
          }
        </p>
      )}
      <div className="flex gap-3">
        <Button
          size="lg"
          className={`flex-1 ${brandSecondary}`}
          disabled={approve.isPending || cancel.isPending}
          onClick={() => cancel.mutate()}
        >
          Cancel
        </Button>
        <Button
          size="lg"
          className={`flex-1 ${brandPrimary}`}
          disabled={approve.isPending || cancel.isPending}
          onClick={() => approve.mutate()}
        >
          {approve.isPending ? "Opening GitHub…" : "Approve"}
        </Button>
      </div>
    </div>
  )
}

function Problem({ error }: { error: Error }) {
  const code = error instanceof ApiError ? error.code : ""
  const expired = code === "github_link_expired"
  return (
    <Outcome
      icon={<XCircleIcon className="size-8 text-white" />}
      title={
        expired ? "This link expired" : code === "github_link_used" ? "Already done" : "Couldn’t open this request"
      }
    >
      {<ErrorMessage error={error} message={githubErrors[code] ?? error.message} />}{" "}
      {code === "github_link_other_account" ? "" : startAgain}
    </Outcome>
  )
}

/**
 * Connecting isn't finished and the next step is on GitHub (the server picks installation or authorization).
 * `auto` goes there at once, so one "Connect GitHub" covers authorizing and installing.
 */
function NextStep({
  title,
  label,
  returnTo,
  auto,
  children,
}: {
  title: string
  label: string
  returnTo: string
  auto: boolean
  children: ReactNode
}) {
  const connect = useGitHubConnect(returnTo)
  const { mutate } = connect
  // Once: a second start would replace the state the first one is leaving with.
  const left = useRef(false)
  useEffect(() => {
    if (!auto || left.current) return
    left.current = true
    mutate()
  }, [auto, mutate])
  const leaving = connect.isPending || connect.isSuccess
  return (
    <>
      <Outcome icon={<GithubLogoIcon className="size-8 text-white" weight="fill" />} title={title}>
        {children}
      </Outcome>
      {connect.isError && (
        <p className="text-sm text-white">
          <ErrorMessage error={connect.error} />
        </p>
      )}
      <Button
        size="lg"
        className={brandPrimary}
        disabled={leaving}
        onClick={() => {
          left.current = true
          mutate()
        }}
      >
        {leaving ? "Opening GitHub…" : label}
      </Button>
    </>
  )
}

/** Connected, but the Paperboat app isn't on the account: its installation (with the repository choice) is next. */
function InstallApp({ login, returnTo, auto }: { login?: string; returnTo: string; auto: boolean }) {
  return (
    <NextStep title="Install the Paperboat app" label="Install on GitHub" returnTo={returnTo} auto={auto}>
      Install it on <span className="font-medium text-white">{login ?? "your GitHub account"}</span> to finish
      connecting GitHub.
    </NextStep>
  )
}

/**
 * Where GitHub's authorization ends (/github/done?result=connected&login=… or result=error&code=…).
 * `return_to` (a console path) offers the way back when the connection started in the console.
 */
export function GitHubDone() {
  const params = useSearchParams()
  const connected = params.get("result") === "connected"
  const login = params.get("login")
  const code = params.get("code") ?? ""
  const back = params.get("return_to")
  const returnTo = back && back.startsWith("/") && !back.startsWith("//") ? back : null
  const status = useQuery({ ...githubStatusQuery, enabled: connected })
  return (
    <BrandScreen>
      <div className="flex flex-col items-center gap-6">
        {connected && status.isPending ? (
          // Not "connected" yet: without the App on the account there is one more step in this tab.
          <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
        ) : connected && status.data?.install_required ? (
          <InstallApp login={status.data.login} returnTo="/config" auto />
        ) : connected ? (
          <Outcome icon={<CheckCircleIcon className="size-8 text-white" />} title="GitHub connected">
            Connected as <span className="font-medium text-white">{login ?? "your GitHub account"}</span>.{" "}
            {returnTo ? "" : "Return to your terminal; you can close this tab."}
          </Outcome>
        ) : (
          <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="GitHub not connected">
            {githubErrors[code] ?? "Something went wrong while connecting."}{" "}
            {returnTo ? "Try again from Config sync." : startAgain}
          </Outcome>
        )}
        {returnTo && (
          <Button size="lg" className={brandPrimary} nativeButton={false} render={<a href={returnTo} />}>
            Back to Config sync
          </Button>
        )}
      </div>
    </BrandScreen>
  )
}

/**
 * GitHub's installation page can send the browser back without a sign-in code: an owner still has to approve, or
 * one more round through GitHub finishes. That round starts by itself only for an account already connected here,
 * so a link to this page can't connect one.
 */
function InstallReturn({ requested }: { requested: boolean }) {
  const status = useQuery({ ...githubStatusQuery, enabled: !requested })
  if (requested)
    return (
      <Outcome icon={<GithubLogoIcon className="size-8 text-white" weight="fill" />} title="Installation requested">
        An owner of that GitHub account has to approve the Paperboat app. Then connect GitHub again.
      </Outcome>
    )
  // Still missing, so it was installed on another account: stay here instead of leaving again by itself.
  if (status.data?.install_required) return <InstallApp login={status.data.login} returnTo="/config" auto={false} />
  return (
    <NextStep
      title="Finish connecting GitHub"
      label="Connect GitHub"
      returnTo="/config"
      auto={Boolean(status.data?.connected)}
    >
      Paperboat finishes this with GitHub.
    </NextStep>
  )
}

/**
 * Where GitHub returns a connect started in the console (/github/callback?code=…&state=…, sent here
 * by the server's registered callback). The exchange is finished through the console, with its
 * session; the result shows here with the way back to where it started.
 */
export function GitHubCallback() {
  const params = useSearchParams()
  const code = params.get("code") ?? ""
  const state = params.get("state") ?? ""
  const declined = params.get("error")
  const setup = params.get("setup_action")
  // Back from the App's installation page without a usable sign-in code: nothing failed, connecting isn't finished.
  const installed = !(code && state) && !declined && Boolean(setup)
  const finish = useQuery({
    ...githubCallbackQuery(code, state),
    enabled: Boolean(code && state && !declined),
  })
  const errorCode =
    declined ?? (finish.error instanceof ApiError ? finish.error.code : finish.error ? "provider_unavailable" : "")
  const back = finish.data?.return_to
  const returnTo = back && back.startsWith("/") && !back.startsWith("//") ? back : "/config"
  return (
    <BrandScreen>
      <div
        className="flex flex-col items-center gap-6"
        data-github-callback={finish.data ? "connected" : errorCode ? "error" : "pending"}
      >
        {finish.data?.install_required ? (
          // Coming back from an installation (setup_action) it is still missing, so it was put elsewhere: no loop.
          <InstallApp login={finish.data.login} returnTo={returnTo} auto={!setup} />
        ) : finish.data ? (
          <Outcome icon={<CheckCircleIcon className="size-8 text-white" />} title="GitHub connected">
            Connected as <span className="font-medium text-white">{finish.data.login ?? "your GitHub account"}</span>.
          </Outcome>
        ) : installed ? (
          <InstallReturn requested={setup === "request"} />
        ) : errorCode || !code || !state ? (
          <Outcome icon={<XCircleIcon className="size-8 text-white" />} title="GitHub not connected">
            {
              <ErrorMessage
                error={finish.error}
                message={
                  githubErrors[errorCode] ?? (finish.error?.message || "GitHub didn’t send the connection back.")
                }
              />
            }{" "}
            Try again from Config sync.
          </Outcome>
        ) : (
          <CircleNotchIcon className="mx-auto size-5 animate-spin text-white/75" />
        )}
        {(finish.data || errorCode || !code || !state) && (
          <Button size="lg" className={brandPrimary} nativeButton={false} render={<a href={returnTo} />}>
            Back to Config sync
          </Button>
        )}
      </div>
    </BrandScreen>
  )
}
