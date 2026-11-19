import { expect, test } from "bun:test"
import { member, parseTree, setItem, setMember, valueOf } from "@/lib/jsonc-edit"

const file = `// who may do what
{
  "rules": [
    // platform runs CI
    {"src": ["group:platform"], "dst": ["tag:ci"], "allow": ["exec"]},
    {"src": ["*"], "dst": ["machine:db"], "deny": ["files"]}, // no files on db
  ],
  /* kept */ "future": {"x": 1},
}
`

const read = (text: string) => {
  const root = parseTree(text)!
  return valueOf(text, root) as Record<string, unknown>
}

test("a file with comments and trailing commas parses; broken files do not", () => {
  expect(read(file).future).toEqual({ x: 1 })
  expect(parseTree("{")).toBeUndefined()
  expect(parseTree('{"a": }')).toBeUndefined()
  expect(parseTree("{} x")).toBeUndefined()
})

test("replacing one rule keeps every comment and key outside it", () => {
  const next = setItem(file, "rules", 1, { src: ["*"], dst: ["machine:db"], allow: ["terminal"] })
  expect(next).toContain("// platform runs CI")
  expect(next).toContain("/* kept */")
  expect(next).toContain('"future"')
  expect((read(next).rules as unknown[])[1]).toEqual({ src: ["*"], dst: ["machine:db"], allow: ["terminal"] })
  expect((read(next).rules as unknown[])[0]).toEqual({ src: ["group:platform"], dst: ["tag:ci"], allow: ["exec"] })
})

test("adding and removing rules", () => {
  const added = setItem(file, "rules", 2, { src: ["*"], dst: ["tag:ci"], allow: ["files"] })
  expect((read(added).rules as unknown[]).length).toBe(3)
  expect(added).toContain("// no files on db")
  const removed = setItem(added, "rules", 0, undefined)
  expect((read(removed).rules as unknown[]).length).toBe(2)
  expect(removed).toContain("// no files on db")
  expect(removed).not.toContain("group:platform")
  const none = setItem(setItem(removed, "rules", 0, undefined), "rules", 0, undefined)
  expect(read(none).rules).toBeUndefined()
  expect(none).toContain("/* kept */")
})

test("keys are set, added and removed at the top level", () => {
  expect(read(setMember("{}", "tags", ["ci"])).tags).toEqual(["ci"])
  expect(read(setMember("{\n  // c\n}", "max_machines", 3)).max_machines).toBe(3)
  const kept = setMember('{"a": 1, // note\n "b": 2}', "a", undefined)
  expect(kept).toContain("// note")
  expect(read(kept)).toEqual({ b: 2 })
  expect(read(setMember(file, "allow_private_access", true)).allow_private_access).toBe(true)
  expect(member(parseTree(file)!, "rules")?.value.kind).toBe("array")
})
