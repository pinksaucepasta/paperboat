// Browser check for F10-06. Paste into the console of a signed-in tab on /environment (personal
// workspace) after setting `window.__vaultPassword` to the dev vault password. Unlocks the
// vault, adds PB_BROWSER_ENV_CHECK with a random value, confirms the name is listed and the value
// and password appear nowhere on the page or in the inputs, then removes it (unless
// `window.__keepVariable` is set, to leave it for a machine to read). Prints the steps.
;(async () => {
  const password = window.__vaultPassword
  delete window.__vaultPassword
  const name = "PB_BROWSER_ENV_CHECK"
  const value = window.__checkValue ?? `check-${crypto.getRandomValues(new Uint32Array(2)).join("")}`
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 60000) => {
    for (let waited = 0; waited < ms; waited += 200) {
      const found = test()
      if (found) return found
      await sleep(200)
    }
    return null
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const button = (label) => [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const page = () => document.body.innerText

  const field = await until(() => document.querySelector('input[aria-label="Vault password"]'))
  pass("unlock form shown", Boolean(field))
  type(field, password)
  await sleep(100)
  button("Unlock").click()
  pass("password input cleared on submit", (await until(() => field.value === "" || null, 2000)) !== null)
  const add = await until(() => button("Add variable"))
  pass("unlocked in browser", Boolean(add), add ? "" : (page().match(/ENV [^\n]*/) ?? [""])[0])
  if (!add) return steps

  add.click()
  const nameInput = await until(() => document.getElementById("env-name"))
  const valueInput = document.getElementById("env-value")
  type(nameInput, name)
  type(valueInput, value)
  await sleep(100)
  button("Save").click()
  pass("value input cleared on submit", (await until(() => valueInput.value === "" || null, 2000)) !== null)
  const listed = await until(() => [...document.querySelectorAll("code")].some((c) => c.textContent === name) || null)
  pass("name listed after set", Boolean(listed))
  pass("value never in page text", !page().includes(value))
  pass("password never in page text", !page().includes(password))
  const html = document.documentElement.outerHTML
  pass("value and password never in DOM", !html.includes(value) && !html.includes(password))

  if (window.__keepVariable) return steps
  document.querySelector(`button[aria-label="Remove ${name}"]`).click()
  const confirm = await until(() =>
    [...document.querySelectorAll("[role=alertdialog] button")].find((b) => b.textContent.trim() === "Remove"),
  )
  confirm?.click()
  const gone = await until(() => ![...document.querySelectorAll("code")].some((c) => c.textContent === name) || null)
  pass("name removed", Boolean(gone))
  return steps
})()
