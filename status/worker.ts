import { handle } from "./lib/api.ts"
import { tick } from "./lib/tick.ts"
import type { Env } from "./lib/types.ts"

// The whole status service in one Worker: static assets (the Astro build, served by the platform before this code runs), the /api/* routes
// and the once-a-minute decision (cron trigger). Anything that is neither an asset nor an API route is a 404 from the assets binding.
export default {
  fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> | Response {
    const { pathname } = new URL(request.url)
    if (pathname.startsWith("/api/")) return handle(request, env, ctx)
    return env.ASSETS ? env.ASSETS.fetch(request) : new Response("not found", { status: 404 })
  },
  async scheduled(_event: ScheduledController, env: Env): Promise<void> {
    await tick(env)
  },
}
