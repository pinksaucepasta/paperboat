import { cloudflareReport } from "./probe.ts"
import { buildSnapshot, evaluate, saveReport } from "./store.ts"
import type { Env } from "./types.ts"

/**
 * One cron minute: the cron Worker's own vantage point reports (an error there is logged and must not stop the decision),
 * then every unit is decided again and the snapshot rebuilt. The cron Worker and the local dev Worker both call this.
 */
export async function tick(env: Env, now = Date.now()): Promise<void> {
  if (env.NET_CHECK_URL) { // a deployment without NET_CHECK_URL (the staging drill) has no cron vantage point
    try {
      const own = await cloudflareReport(env)
      await saveReport(env.DB, own.vantage, now, own.probeTs, own.netOk, own.build, own.results)
    } catch (e) {
      console.error("status cron vantage", e instanceof Error ? e.message : e)
    }
  }
  await evaluate(env.DB, now) // if this throws the cron run fails visibly and the page goes 'delayed' by itself; nothing refreshes `at`
  await buildSnapshot(env.DB, now)
}
