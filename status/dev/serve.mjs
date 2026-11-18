// Local demo server: serves dist/ and forwards /api/* to a running `wrangler dev` (same-origin, like Pages Functions).
// usage: node dev/serve.mjs [port] [worker origin]
import { createServer, request } from "node:http"
import { readFileSync, statSync } from "node:fs"
import { extname, join, normalize } from "node:path"
import { fileURLToPath } from "node:url"
const port = Number(process.argv[2] ?? 4330), worker = new URL(process.argv[3] ?? "http://127.0.0.1:8787")
const root = join(fileURLToPath(new URL(".", import.meta.url)), "..", "dist")
const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2" }
createServer((req, res) => {
  if (req.url.startsWith("/api/")) {
    const up = request({ host: worker.hostname, port: worker.port, path: req.url, method: req.method, headers: req.headers }, (r) => { res.writeHead(r.statusCode, r.headers); r.pipe(res) })
    up.on("error", () => { res.writeHead(502); res.end("the Worker is not running") })
    return req.pipe(up)
  }
  let file = join(root, normalize(req.url.split("?")[0]))
  try { if (statSync(file).isDirectory()) file = join(file, "index.html") } catch { /* 404 below */ }
  let body
  try { body = readFileSync(file) } catch { res.writeHead(404); return res.end("not found") }
  res.writeHead(200, { "content-type": types[extname(file)] ?? "application/octet-stream" })
  res.end(body)
}).listen(port, "127.0.0.1")
