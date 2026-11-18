import { tick } from "../lib/tick.ts"
import type { Env } from "../lib/types.ts"

// Pages Functions have no cron triggers, so the once-a-minute decision lives in this tiny Worker on the same D1 database.
export default {
  async scheduled(_event: ScheduledController, env: Env): Promise<void> {
    await tick(env)
  },
}
