import { expect, test } from "bun:test"
import { installCommand, installUrlFrom } from "@/lib/enrollment"

const url = "https://get.example.test"

test("the install address comes from the server's bootstrap command", () => {
  expect(installUrlFrom("curl -fsSL https://get.example.test | sh")).toBe(url)
  expect(installUrlFrom(undefined)).toBeUndefined()
})

test("one address for both shells, with the token in the installer's environment", () => {
  expect(installCommand(url, "posix")).toBe("curl -fsSL https://get.example.test | sh")
  expect(installCommand(url, "powershell")).toBe("irm https://get.example.test | iex")
  expect(installCommand(url, "posix", "TOKEN", "studio")).toBe("curl -fsSL https://get.example.test | PB_TOKEN='TOKEN' PB_NAME='studio' sh")
  expect(installCommand(url, "powershell", "TOKEN")).toBe("$env:PB_TOKEN='TOKEN'; irm https://get.example.test | iex")
  // A name without a token has nothing to attach to, and the token never goes in the address.
  expect(installCommand(url, "posix", undefined, "studio")).toBe("curl -fsSL https://get.example.test | sh")
  expect(installCommand(url, "posix", "TOKEN")).not.toContain("?p=")
})
