"use client"

import { CheckCircleIcon, CircleNotchIcon, GithubLogoIcon, XCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useSearchParams } from "next/navigation"
import { useSyncExternalStore } from "react"
import { ApiError } from "@/lib/api"
import { approveLink, cancelLink, githubErrors, reviewLink } from "@/lib/github"
import { useTimeFormat } from "@/lib/use-time-format"
import { BrandScreen, Heading, Outcome, brandPrimary, brandSecondary } from "@/components/brand-screen"
import { Button } from "@/components/ui/button"

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
  const review = useQuery({ queryKey: ["github-link", link], queryFn: () => reviewLink(link, token), retry: false })
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
      <p className="text-sm text-white/75">
        Paperboat keeps your config files in a private GitHub repository you choose. Approve only if you just ran this from
        your terminal.
      </p>
      {failed && <p className="text-sm text-white">{failed instanceof ApiError ? (githubErrors[failed.code] ?? failed.message) : failed.message}</p>}
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
      title={expired ? "This link expired" : code === "github_link_used" ? "Already done" : "Couldn’t open this request"}
    >
      {githubErrors[code] ?? error.message} {code === "github_link_other_account" ? "" : startAgain}
    </Outcome>
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
  return (
    <BrandScreen>
      <div className="flex flex-col items-center gap-6">
        {connected ? (
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
