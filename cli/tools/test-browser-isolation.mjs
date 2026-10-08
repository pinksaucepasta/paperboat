// Usage: PAPERBOAT_PLAYWRIGHT_MODULE=/installed/playwright/index.mjs node
// tools/test-browser-isolation.mjs <loopback-CDP-url> <fixture-json>
// Chrome must use a disposable profile, exact fixture SPKI exception and
// host-resolver mapping to loopback. No system trust or user profile is changed.
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {pathToFileURL} from 'node:url';
const {chromium}=await import(pathToFileURL(process.env.PAPERBOAT_PLAYWRIGHT_MODULE).href);
const fixture=JSON.parse(await readFile(process.argv[3],'utf8'));
const browser=await chromium.connectOverCDP(process.argv[2]);
const context=await browser.newContext();
const [a,b]=fixture.origins;
try {
 const page=await context.newPage();
 await page.goto(a);
 await page.evaluate(other=>{
  document.cookie='jsA=A; Secure; Path=/; SameSite=Strict';
  document.cookie='badJS=A; Domain=pprbt; Secure; Path=/';
  document.cookie=`badSibling=A; Domain=${new URL(other).hostname}; Secure; Path=/`;
 },b);
 let cookies=await context.cookies();
 assert(cookies.some(c=>c.name==='headerA'));
 assert(cookies.some(c=>c.name==='jsA'));
 assert(!cookies.some(c=>['badParent','badJS','badSibling'].includes(c.name)));
 await page.goto(b);
 assert(!await page.evaluate(()=>document.cookie.includes('headerA=')||document.cookie.includes('jsA=')));
 const same=await page.evaluate(()=>fetch('/echo',{credentials:'include'}).then(r=>r.json()));
 assert(same.cookie.includes('headerB=B'));
 assert.equal(same.site,'same-origin');
 await page.goto(a);
 const cross=await page.evaluate(other=>fetch(other+'/echo',{credentials:'include'}).then(r=>r.json()),b);
 assert.equal(cross.site,'cross-site');
 assert(!cross.cookie.includes('headerB='));
 console.log(JSON.stringify({result:'PASS',browser:browser.version(),checks:['header cookie isolation','JavaScript cookie isolation','parent and sibling Domain rejection','same-origin cookie delivery','cross-site SameSite=Strict exclusion']}));
 await page.goto(a+'/finish');
} finally { await context.close(); await browser.close(); }
