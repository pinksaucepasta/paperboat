// Browser check for connecting GitHub from the console (Config sync, /github/connect, /github/done)
// against the live server. Paste into a signed-in tab of the console. It reads GET /v1/github/status,
// checks Config sync shows it; starts a console connect (POST /v1/github/oauth/start) without
// following it and checks it leads to GitHub's authorization page and that a foreign return_to is
// refused; opens /github/connect with an incomplete and an unknown link; and renders /github/done
// for a success, an error and a return to Config sync. It never authorizes anything on GitHub or
// disconnects an existing connection. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const body = () => document.body.innerText.replace(/\s+/g, " ")
  const go = async (path, ready) => {
    window.next.router.push(path)
    return until(() => location.pathname === path.split(/[?#]/)[0] && ready())
  }

  // Status, and Config sync showing it.
  const status = (await (await fetch("/api/v1/github/status")).json()).data
  pass("GET github/status answers", typeof status?.connected === "boolean", status?.connected ? `connected as ${status.login}` : "not connected")
  const section = await go("/config", () => document.querySelector("[data-github]"))
  pass(
    "Config sync shows the GitHub connection as the server does",
    section?.dataset.github === (status.connected ? "connected" : "disconnected") &&
      (!status.connected || body().includes(`Connected as ${status.login}`)),
  )
  const action = [...document.querySelectorAll("main button")].find((b) => /^(Connect GitHub|Disconnect)$/.test(b.textContent.trim()))
  pass("it offers Connect GitHub or Disconnect", Boolean(action) && action.textContent.trim() === (status.connected ? "Disconnect" : "Connect GitHub"))

  // Starting from the console leads to GitHub; a foreign return_to is refused.
  const start = async (returnTo) => {
    // The console's /api proxy adds the session's CSRF header, as for every console POST.
    const response = await fetch("/api/v1/github/oauth/start", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ return_to: returnTo }),
    })
    return { status: response.status, body: await response.json().catch(() => null) }
  }
  const ok = await start("/config")
  const url = ok.body?.data?.authorization_url ?? ""
  pass("Connect starts GitHub's authorization", ok.status === 200 && /^https:\/\/github\.com\/login\/oauth\/authorize\?/.test(url), `${ok.status}`)
  const foreign = await start("//example.test/x")
  pass("a return_to outside the console is refused", foreign.status === 400 && foreign.body?.error?.code === "invalid_return_to", `${foreign.status} ${foreign.body?.error?.code ?? ""}`)

  // pb's link: incomplete, and unknown.
  await go("/github/connect?link=check", () => /incomplete/.test(body()))
  pass("a link without its secret says it is incomplete", /This link is incomplete/.test(body()))
  window.location.hash = "not-a-real-token"
  window.next.router.push("/github/connect?link=github_link_check_unknown#not-a-real-token")
  const unknown = await until(() => /isn’t valid anymore|expired|another Paperboat account|Couldn’t open/.test(body()) && body())
  pass("an unknown link says so and how to start again", Boolean(unknown) && /Run the pb command again/.test(unknown), unknown?.slice(0, 120) ?? "")

  // GitHub's result.
  await go("/github/done?login=octocat&result=connected", () => /GitHub connected/.test(body()))
  pass("success names the GitHub account", /Connected as octocat/.test(body()) && /Return to your terminal/.test(body()))
  await go("/github/done?code=github_scope_denied&result=error", () => /GitHub not connected/.test(body()))
  pass("an error says what went wrong and how to start again", /didn’t grant the access/.test(body()) && /Run the pb command again/.test(body()))
  await go("/github/done?login=octocat&result=connected&return_to=%2Fconfig", () => /Back to Config sync/.test(body()))
  pass("a console-started connect offers the way back", [...document.querySelectorAll("a")].some((a) => a.getAttribute("href") === "/config"))
  await go("/github/done?login=x&result=connected&return_to=%2F%2Fexample.test", () => /GitHub connected/.test(body()))
  pass("a foreign return_to is not offered", ![...document.querySelectorAll("a")].some((a) => /example\.test/.test(a.href)))
  return steps
})()
