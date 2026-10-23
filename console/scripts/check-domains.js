// Browser check for the Domains page: one row per domain name, and no React key warnings. Paste
// into a signed-in tab of the console. Seeds this browser's stored machine domains with a
// duplicate of Paperboat's own domain (the cause of "two children with the same key"), opens
// /domains, and checks each name is listed once and React logged no key warning; then restores
// the stored list. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const key = "pb-machine-domains"
  const saved = localStorage.getItem(key)
  const warnings = []
  const original = console.error
  console.error = (...args) => {
    const text = args.map(String).join(" ")
    if (/same key|unique "key"/.test(text)) warnings.push(text.slice(0, 160))
    original(...args)
  }
  try {
    const config = (await (await fetch("/api/v1/client-configuration")).json()).data
    const managed = config?.local_access_base_domain
    pass("the managed machine domain is known", Boolean(managed), managed ?? "")
    // The stored list holds the managed name twice, as a browser could after adding it early.
    localStorage.setItem(key, JSON.stringify({ added: [managed, managed, "check.example.test"], primary: "" }))
    window.next.router.push("/domains")
    await sleep(6000)
    const names = [...document.querySelectorAll("main tbody tr td:first-child")].map((cell) => cell.innerText.trim().split("\n")[0])
    const duplicates = names.filter((name, index) => names.indexOf(name) !== index)
    pass("every domain is listed once", names.length > 0 && duplicates.length === 0, duplicates.join(", ") || `${names.length} rows`)
    pass("Paperboat's domain is listed", names.includes(managed))
    pass("React logged no duplicate-key warning", warnings.length === 0, warnings[0] ?? "")
  } finally {
    console.error = original
    if (saved === null) localStorage.removeItem(key)
    else localStorage.setItem(key, saved)
  }
  return steps
})()
