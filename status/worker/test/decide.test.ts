import { test } from "node:test"
import assert from "node:assert/strict"
import { decideComponent, decideUnit, type Sample, type Series } from "../src/decide.ts"

const NOW = 10_000_000
const SLOW = 2000
// minutes ago -> sample; pattern is oldest first, "." pass, "x" fail, "s" slow pass
function series(vantage: string, pattern: string, newestAgoMs = 5000): Series {
  const samples: Sample[] = [...pattern].map((c, i, all) => ({
    ts: NOW - newestAgoMs - (all.length - 1 - i) * 60_000,
    ok: c !== "x",
    ms: c === "s" ? 5000 : 100,
  }))
  return { vantage, samples }
}
const decide = (prev: "operational" | "degraded" | "major" | "delayed", ...s: Series[]) => decideUnit(prev, NOW, s, SLOW).state

test("one vantage failing three times is not red", () => {
  assert.equal(decide("operational", series("oracle", "..xxx"), series("anvit", ".....")), "operational")
})
test("two vantages failing three in a row is red", () => {
  assert.equal(decide("operational", series("oracle", "..xxx"), series("anvit", "xxx")), "major")
})
test("two failures in a row are not enough", () => {
  assert.equal(decide("operational", series("oracle", "...xx"), series("anvit", "...xx")), "operational")
})
test("three failures from vantages that are not consecutive samples do not count", () => {
  assert.equal(decide("operational", series("oracle", "x.x.x"), series("anvit", "x.x.x")), "operational")
})
test("a vantage with no result for 3 minutes does not count towards red, and its lone witness is degraded, not green", () => {
  assert.equal(decide("operational", series("oracle", "xxx"), series("anvit", "xxx", 200_000)), "degraded")
  assert.equal(decide("operational", series("oracle", "....."), series("anvit", "xxx", 200_000)), "operational")
})
test("latency over the threshold on two vantages is degraded", () => {
  assert.equal(decide("operational", series("oracle", "sss"), series("anvit", "sss")), "degraded")
})
test("one failing and one slow vantage is degraded, not red", () => {
  assert.equal(decide("operational", series("oracle", "xxx"), series("anvit", "sss")), "degraded")
})
test("all vantages stale is delayed, never green", () => {
  assert.equal(decide("operational", series("oracle", ".....", 200_000), series("anvit", ".....", 400_000)), "delayed")
  assert.equal(decide("operational"), "delayed")
})
test("red stays until five passes in a row on two vantages", () => {
  // four passes since the failures: not green yet
  assert.equal(decide("major", series("oracle", "xxx...."), series("anvit", "xxx....")), "degraded")
  assert.equal(decide("major", series("oracle", "xxx....."), series("anvit", "xxx.....")), "operational")
})
test("red recovering on one vantage only is not green", () => {
  assert.equal(decide("major", series("oracle", "....."), series("anvit", "xxx")), "degraded")
})
test("degraded needs five passes too", () => {
  assert.equal(decide("degraded", series("oracle", "ssss."), series("anvit", "ssss.")), "degraded")
  assert.equal(decide("degraded", series("oracle", "....."), series("anvit", ".....")), "operational")
})
test("a single covering vantage can recover alone but can never make a unit red", () => {
  assert.equal(decide("operational", series("cf", "xxxxx")), "degraded")
  assert.equal(decide("degraded", series("cf", ".....")), "operational")
})
test("a delayed unit that returns healthy is green only after its first view is healthy", () => {
  assert.equal(decide("delayed", series("oracle", "....."), series("anvit", ".....")), "operational")
})
test("a delayed unit is not green on unconfirmed data: it stays delayed until five passes, or is degraded when a witness fails", () => {
  assert.equal(decide("delayed", series("oracle", "x"), series("anvit", "x")), "delayed")
  assert.equal(decide("delayed", series("oracle", "..."), series("anvit", "...")), "delayed")
  assert.equal(decide("delayed", series("oracle", "xxx"), series("anvit", "xxx")), "major")
  assert.equal(decide("delayed", series("oracle", "xxx"), series("anvit", "xxx", 400_000)), "degraded")
})
test("component: all units down is major, some down is degraded", () => {
  assert.equal(decideComponent(["major", "major"]), "major")
  assert.equal(decideComponent(["major", "operational"]), "degraded")
  assert.equal(decideComponent(["degraded", "operational"]), "degraded")
  assert.equal(decideComponent(["operational", "delayed"]), "delayed")
  assert.equal(decideComponent(["major", "delayed"]), "degraded")
  assert.equal(decideComponent(["operational", "operational"]), "operational")
  assert.equal(decideComponent([]), "delayed")
})
