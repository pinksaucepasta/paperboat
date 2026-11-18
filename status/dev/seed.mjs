// Local demo data for the page: 90 days of uptime, incidents in three months, upcoming maintenance, and fresh passing reports.
// usage: node dev/seed.mjs [degraded-unit] > seed.sql   (then wrangler d1 execute --local --file seed.sql; GET /cdn-cgi/handler/scheduled)
import catalog from "../config/components.ts"
const now = Date.now(), DAY = 86_400_000, q = (s) => `'${String(s).replace(/'/g, "''")}'`
const out = []
let n = 7
const rnd = () => ((n = (n * 1103515245 + 12345) % 2147483648) / 2147483648)
for (const c of catalog.components) {
  for (let i = 89; i >= 0; i--) {
    const day = new Date(now - i * DAY).toISOString().slice(0, 10)
    const r = rnd()
    const down = r > 0.985 ? 25 : 0, degraded = r > 0.93 && r <= 0.985 ? 18 : 0, delayed = r < 0.01 ? 40 : 0
    out.push(`INSERT INTO uptime_day (component, day, up, degraded, down, delayed) VALUES (${q(c.id)}, ${q(day)}, ${1440 - down - degraded - delayed}, ${degraded}, ${down}, ${delayed});`)
  }
}
const inc = (id, title, status, comps, daysAgo, hours, updates) => {
  const created = now - daysAgo * DAY, resolved = status === "resolved" ? created + hours * 3600_000 : null
  out.push(`INSERT INTO incidents (id, title, status, components, created, updated, resolved) VALUES (${q(id)}, ${q(title)}, ${q(status)}, ${q(JSON.stringify(comps))}, ${created}, ${created + hours * 3600_000}, ${resolved ?? "NULL"});`)
  updates.forEach(([st, body, offsetMin], k) => out.push(`INSERT INTO incident_updates (id, incident_id, status, body, ts) VALUES (${q(`${id}-${k}`)}, ${q(id)}, ${q(st)}, ${q(body)}, ${created + offsetMin * 60_000});`))
}
inc("inc-a", "Elevated sign-in errors", "resolved", ["api"], 3, 1.2, [
  ["investigating", "Some people see an error when signing in. Machines that are already connected are not affected.", 0],
  ["identified", "A database failover caused the errors. We are moving traffic back.", 25],
  ["resolved", "Sign-in is back to normal. The window was 14:03 to 15:15; about 4% of sign-ins failed at the peak.", 72]])
inc("inc-b", "Relay in India slow to accept connections", "resolved", ["relays"], 21, 3, [
  ["investigating", "Machines that cannot connect directly may take longer to connect.", 0],
  ["monitoring", "We shifted load to the second relay and are watching latency.", 90],
  ["resolved", "Latency is back to normal. Direct connections were never affected.", 180]])
inc("inc-c", "Previews returned errors for some tunnels", "resolved", ["edges"], 47, 2, [
  ["investigating", "Some preview and tunnel URLs return a 502 error.", 0],
  ["identified", "One edge was out of memory. We restarted it.", 40],
  ["resolved", "Previews and tunnels are working again.", 120]])
if (process.argv[2] === "open") inc("inc-open", "File transfers are slower than usual", "monitoring", ["transfers"], 0.04, 1, [
  ["investigating", "Sending files between machines is slower than usual. Transfers still complete.", 0],
  ["monitoring", "We have added capacity and are watching the recovery.", 30]])
out.push(`INSERT INTO maintenance (id, title, body, components, starts, ends, created) VALUES ('m1', 'Relay upgrade', 'Machines that need a relay may reconnect once. Direct connections are not affected.', ${q(JSON.stringify(["relays"]))}, ${now + 2 * DAY}, ${now + 2 * DAY + 2 * 3600_000}, ${now});`)
for (let i = 5; i >= 0; i--) for (const v of catalog.vantages) {
  const results = Object.fromEntries(catalog.components.flatMap((c) => c.units).filter((u) => u.vantages.includes(v)).map((u) => [u.id, [process.argv[2] === "degraded" && u.id === "relay_india_1" ? 0 : 1, 120]]))
  const ts = now - i * 60_000 - 5000
  out.push(`INSERT OR REPLACE INTO reports (vantage, bucket, ts, probe_ts, net_ok, build, results) VALUES (${q(v)}, ${Math.floor(ts / 60000) * 60000}, ${ts}, ${ts}, 1, 'seed', ${q(JSON.stringify(results))});`)
}
console.log(out.join("\n"))
