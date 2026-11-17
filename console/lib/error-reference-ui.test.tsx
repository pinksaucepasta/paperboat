import { expect, test } from "bun:test"
import { renderToStaticMarkup } from "react-dom/server"
import { ApiError } from "@/lib/api"
import { ErrorMessage } from "@/components/error-reference"
import { LoadError } from "@/components/load-error"

const reference = "support_0b7c3a52-9d1e-4c8a-8f2b-3e5d7a9c1b64"

test("every error state shows its reference with a copy button: a form error, a failed load, a refusal", () => {
  const server = new ApiError(503, "unavailable", "Paperboat is temporarily unavailable.", undefined, undefined, {
    reference,
  })
  const form = renderToStaticMarkup(<ErrorMessage error={server} />)
  expect(form).toContain("Paperboat is temporarily unavailable.")
  expect(form).toContain(`Reference:`)
  expect(form).toContain(reference)
  expect(form).toContain('aria-label="Copy reference"')

  const load = renderToStaticMarkup(<LoadError what="machines" error={server} retry={() => {}} />)
  expect(load).toContain("Couldn’t load machines.")
  expect(load).toContain(reference)

  const refusal = renderToStaticMarkup(
    <LoadError
      what="usage"
      error={new ApiError(403, "team_sso_required", "Use SSO.", undefined, undefined, { reference })}
      retry={() => {}}
    />,
  )
  expect(refusal).toContain("Use SSO.")
  expect(refusal).toContain(reference)

  // No server answered: a client reference stands in, in the same place.
  const offline = renderToStaticMarkup(<ErrorMessage error={new ApiError(0, "network", "Couldn’t reach Paperboat.")} />)
  expect(offline).toMatch(/Reference:.*client_[0-9a-f]{32}/)
})
