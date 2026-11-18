import { handle } from "../../lib/api.ts"
import type { Env } from "../../lib/types.ts"

// The status API, same-origin with the page: Cloudflare Pages Functions (D1 binding DB, secrets in the project's environment).
export const onRequest: PagesFunction<Env> = (context) => handle(context.request, context.env, context)
