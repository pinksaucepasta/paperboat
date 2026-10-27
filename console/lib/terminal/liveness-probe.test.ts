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
      set: (fn, ms) => (timers.push({ fn, ms, live: true }) - 1) as unknown as ReturnType<typeof setTimeout>,
      clear: (id) => {
        timers[id as unknown as number].live = false
      },
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

  test("a reply handled before the send returned its id does not leave a probe to time out", async () => {
    const timers: unknown[] = []
    let silent = 0
    const probe = createLivenessProbe(
      async () => {
        probe.answered("probe_early") // the reply is handled while `send` is still resolving
        return "probe_early"
      },
      () => silent++,
      { set: () => timers.push(1) as unknown as ReturnType<typeof setTimeout>, clear: () => undefined },
    )
    await probe.start()
    expect(timers.length).toBe(0)
    expect(silent).toBe(0)
  })

  test("the default timers are real ones that fire once, detached from any object", async () => {
    let silent = 0
    const probe = createLivenessProbe(async () => "probe_real", () => silent++)
    const realSet = globalThis.setTimeout
    globalThis.setTimeout = ((run: () => void) => realSet(run, 0)) as typeof setTimeout // the 5 s deadline, shortened
    try {
      await probe.start()
      await new Promise((resolve) => realSet(resolve, 20))
    } finally {
      globalThis.setTimeout = realSet
    }
    expect(silent).toBe(1)
  })
})
