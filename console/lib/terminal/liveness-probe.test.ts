import { describe, expect, test } from "bun:test"
import { createLivenessProbe, probeDeadlineMs } from "./liveness-probe"

function setup(request: string | null = "probe_1") {
  const timers: { fn: () => void; ms: number; live: boolean }[] = []
  let silent = 0
  let sent = 0
  const probe = createLivenessProbe(
    async () => {
      sent++
      return request ?? undefined
    },
    () => silent++,
    {
      set: ((fn: () => void, ms: number) => timers.push({ fn, ms, live: true }) - 1) as unknown as typeof setTimeout,
      clear: ((id: number) => {
        timers[id].live = false
      }) as unknown as typeof clearTimeout,
    },
  )
  const fire = () => timers.forEach((t) => t.live && ((t.live = false), t.fn()))
  return { probe, fire, timers, silent: () => silent, sent: () => sent }
}

describe("liveness probe", () => {
  test("silence for the deadline reports the connection dead, once", async () => {
    const t = setup()
    await t.probe.start()
    expect(t.timers[0].ms).toBe(probeDeadlineMs)
    t.fire()
    t.fire()
    expect(t.silent()).toBe(1)
  })

  test("the matching reply, or any traffic, clears it; another probe's reply does not", async () => {
    const t = setup()
    await t.probe.start()
    t.probe.answered("probe_other")
    t.fire()
    expect(t.silent()).toBe(1)
    await t.probe.start()
    t.probe.answered("probe_1")
    t.fire()
    expect(t.silent()).toBe(1)
    await t.probe.start()
    t.probe.answered()
    t.fire()
    expect(t.silent()).toBe(1)
  })

  test("a probe in flight is not doubled, and a probe that could not be sent starts no timer", async () => {
    const t = setup()
    await Promise.all([t.probe.start(), t.probe.start()])
    await t.probe.start()
    expect(t.sent()).toBe(1)
    const none = setup(null)
    await none.probe.start()
    expect(none.timers.length).toBe(0)
  })
})
