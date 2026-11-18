// Local view of the page: serves site/ and forwards /api/* to a running `wrangler dev` Worker.
// usage: node status/dev-server.mjs <port> <worker origin>
import { createServer, request } from "node:http"
import { readFileSync } from "node:fs"
import { extname, join, normalize } from "node:path"
import { fileURLToPath } from "node:url"
const [port, worker] = [Number(process.argv[2] ?? 8788), new URL(process.argv[3] ?? "http://127.0.0.1:8787")]
const root = join(fileURLToPath(new URL(".", import.meta.url)), "site")
const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css" }
createServer((req, res) => {
  if (req.url.startsWith("/api/")) {
    const up = request({ host: worker.hostname, port: worker.port, path: req.url, method: req.method, headers: req.headers }, (r) => { res.writeHead(r.statusCode, r.headers); r.pipe(res) })
    return req.pipe(up)
  }
  const file = join(root, normalize(req.url.split("?")[0]).replace(/^\/$/, "/index.html"))
  try { res.writeHead(200, { "content-type": types[extname(file)] ?? "application/octet-stream" }); res.end(readFileSync(file)) } catch { res.writeHead(404); res.end() }
}).listen(port, "127.0.0.1")
