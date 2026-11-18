import { handle } from "../lib/api.ts"
import { tick } from "../lib/tick.ts"
import type { Env } from "../lib/types.ts"

// Local only (the drill): the Pages Function and the cron Worker in one process, sharing one local D1.
export default {
  fetch: (request: Request, env: Env, ctx: ExecutionContext) => handle(request, env, ctx),
  scheduled: (_event: ScheduledController, env: Env) => tick(env),
}
