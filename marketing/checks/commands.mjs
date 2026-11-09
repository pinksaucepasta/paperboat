// Every pb and pbh command the site shows is checked against the real CLI, so a command that
// would not run as written cannot be published. pb is asked for its own definition
// (`pb --json` lists the commands, `pb <command> --help --json` gives usage and flags); pbh has
// no such output, so its subcommands and flags are read from helper/cmd/pbh.
//
// Checked: the command exists, every flag exists, flags marked required are present, and enough
// positional arguments are given for the usage line. Placeholders such as <tunnel> count as
// arguments. A command inside a sentence (<code>pb tunnel status</code>) is checked for its name
// and flags only, because prose often names a command without its arguments.
import { spawnSync } from "node:child_process"
import { readFileSync, readdirSync } from "node:fs"
import { join } from "node:path"

/**
 * Requirements a command enforces in its own code, which its help cannot show. Each names the
 * file that enforces it; add a line here when the CLI gains another.
 */
const enforcedInCode = {
  // cli/cmd/pb/terminal_sharing_command.go: "share requires --team, exactly one of --all or --member"
  "pb session share": (flags) => (flags.has("team") && flags.has("all") !== flags.has("member") ? "" : "needs --team and exactly one of --all or --member"),
  // cli/cmd/pb/main.go transferClientForCommand: "--on is required"
  "pb send list": (flags) => (flags.has("on") ? "" : "needs --on <machine>"),
  "pb send status": (flags) => (flags.has("on") ? "" : "needs --on <machine>"),
  "pb send cancel": (flags) => (flags.has("on") ? "" : "needs --on <machine>"),
}

const unescape = (html) =>
  html.replace(/<[^>]+>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&#39;|&apos;/g, "'").replace(/&amp;/g, "&")

/** Shell-like words: quotes group, nothing is expanded. */
const words = (line) => [...line.matchAll(/'[^']*'|"(?:[^"\\]|\\.)*"|[^\s'"]+(?:'[^']*'|"(?:[^"\\]|\\.)*"|[^\s'"]+)*/g)].map((match) => match[0])

/** The pb and pbh invocations in one shell line: `a | pb x && pbh y` gives two. */
const invocations = (line) => {
  const found = []
  let current = []
  for (const word of [...words(line), ";"]) {
    if (["|", "&&", "||", ";"].includes(word)) {
      if (current[0] === "pb" || current[0] === "pbh") found.push(current)
      current = []
    } else current.push(word)
  }
  return found
}

/** Every command on a page: whole lines of a transcript or a listing, and commands named in prose. */
function commandsOn(html) {
  const found = []
  for (const [, block] of html.matchAll(/<pre[^>]*>([\s\S]*?)<\/pre>/g))
    for (const raw of block.split("\n")) {
      const prompt = raw.match(/<span class="p">([^<]*)<\/span> <span class="r">([\s\S]*?)<\/span>$/)
      const text = prompt ? (prompt[1] === "$" ? unescape(prompt[2]) : "") : unescape(raw).trim()
      for (const argv of invocations(text.replace(/^\$ /, ""))) found.push({ argv, whole: true, text })
    }
  const prose = html.replace(/<pre[^>]*>[\s\S]*?<\/pre>/g, "")
  for (const [, inner] of prose.matchAll(/<code[^>]*>([^<]*)<\/code>/g)) {
    const text = unescape(inner).trim()
    // "pb help, pb <command> --help" names two commands; "pb completion bash|zsh|fish" names one.
    for (const part of text.split(/,\s+(?=pbh? )/)) for (const argv of invocations(part)) found.push({ argv, whole: false, text })
  }
  return found
}

export function checkCommands(pages, fail, { pb, repo }) {
  const ask = (args) => {
    const run = spawnSync(pb, args, { encoding: "utf8", timeout: 20_000 })
    try {
      return JSON.parse(run.stdout).data
    } catch {
      throw new Error(`site-check: ${pb} ${args.join(" ")} did not print JSON: ${run.stderr || run.error}`)
    }
  }
  const catalog = new Set(ask(["--json"]).commands.map((entry) => entry.command))
  const hasChildren = (path) => [...catalog].some((command) => command.startsWith(path + " "))
  const helps = new Map()
  const help = (path) => helps.get(path) ?? helps.set(path, ask([...path.split(" ").slice(1), "--help", "--json"])).get(path)

  // pbh: `case "install":` names a subcommand, fs.String("name", …) a flag.
  const helper = readdirSync(join(repo, "helper/cmd/pbh"))
    .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
    .map((name) => readFileSync(join(repo, "helper/cmd/pbh", name), "utf8"))
    .join("\n")
  const helperCommands = new Set([...helper.matchAll(/\bcase ((?:"[a-z-]+",? ?)+):/g)].flatMap((match) => [...match[1].matchAll(/"([a-z-]+)"/g)].map((name) => name[1])))
  const helperFlags = new Set([...helper.matchAll(/\bfs\.(?:String|Bool|Int|Duration)\("([a-z-]+)"/g)].map((match) => match[1]))

  let count = 0
  for (const [page, html] of pages)
    for (const { argv, whole, text } of commandsOn(html)) {
      if (argv.length === 1) continue // the program's name alone, in a sentence
      count++
      const wrong = (message) => fail(page, `command would not run: \`${text}\`: ${message}`)
      const end = argv.indexOf("--")
      const own = end < 0 ? argv.slice(1) : argv.slice(1, end)
      const payload = end < 0 ? [] : argv.slice(end + 1)

      if (argv[0] === "pbh") {
        if (!helperCommands.has(own[0])) wrong(`pbh has no "${own[0]}" command`)
        for (const word of own.filter((item) => item.startsWith("--"))) if (!helperFlags.has(word.slice(2).split("=")[0])) wrong(`pbh has no flag ${word}`)
        continue
      }

      // The command is the longest run of leading words that names one; the rest are arguments.
      let path = "pb"
      let at = 0
      while (at < own.length && !own[at].startsWith("-") && (catalog.has(`${path} ${own[at]}`) || hasChildren(`${path} ${own[at]}`))) path += ` ${own[at++]}`
      const info = help(path)
      const passthrough = info.usage.includes("<standard") // scp, sftp, rsync take the tool's own flags
      const flags = new Map(info.flags.map((flag) => [flag.name, flag]))
      const short = new Map(info.flags.filter((flag) => flag.shorthand).map((flag) => [flag.shorthand, flag]))
      const given = new Set()
      const positional = [...payload]
      for (let i = at; i < own.length; i++) {
        const word = own[i]
        if (!word.startsWith("-") || passthrough) {
          positional.push(word)
          continue
        }
        const name = word.replace(/^--?/, "").split("=")[0]
        const flag = word.startsWith("--") ? flags.get(name) : short.get(name)
        if (name === "help") continue
        if (!flag) {
          wrong(`${path} has no flag ${word.split("=")[0]}`)
          continue
        }
        given.add(flag.name)
        if (flag.type !== "bool" && !word.includes("=")) i++ // its value
      }

      // A word after a command group that takes no arguments is a subcommand that does not exist.
      if (path !== "pb" && positional.length && hasChildren(path) && info.usage === `${path} [flags]`) wrong(`${path} has no "${positional[0]}" command`)
      if (!whole) continue
      for (const flag of info.flags) if (flag.required && !given.has(flag.name)) wrong(`${path} requires --${flag.name}`)
      // "<path>... --to <machine>": a placeholder right after a flag is that flag's value, not an argument.
      const usage = info.usage.slice(path.length).split(/\s+/)
      const needed = usage.filter((part, index) => part.includes("<") && !part.startsWith("[") && !/^--\w/.test(usage[index - 1] ?? "")).length
      if (positional.length < needed) wrong(`${path} needs ${needed} argument(s): ${info.usage}`)
      const missing = enforcedInCode[path]?.(given)
      if (missing) wrong(`${path} ${missing}`)
    }
  return count
}
