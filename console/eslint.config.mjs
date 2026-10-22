import { defineConfig, globalIgnores } from "eslint/config"
import nextVitals from "eslint-config-next/core-web-vitals"
import nextTs from "eslint-config-next/typescript"

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  // Rendering must not read the clock, zone or locale directly: the server and the browser's
  // hydrating render would disagree (a hydration error). Times go through useTimeFormat/useNow
  // (lib/use-time-format.ts), which render a stable form until hydrated. Code that runs only in
  // event handlers or effects may disable this per line with the reason.
  {
    files: ["app/**/*.{ts,tsx}", "components/**/*.{ts,tsx}", "hooks/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector: "CallExpression[callee.object.name='Date'][callee.property.name='now']",
          message: "Date.now() in a component differs between server and browser; use useNow() or useTimeFormat().",
        },
        {
          selector: "NewExpression[callee.name='Date'][arguments.length=0]",
          message: "new Date() in a component differs between server and browser; use useNow() or useTimeFormat().",
        },
        {
          selector: "CallExpression[callee.property.name=/^toLocale(Date|Time)?String$/]",
          message: "toLocale*String depends on the viewer's locale and zone; use useTimeFormat().",
        },
        {
          selector: "NewExpression[callee.object.name='Intl'][callee.property.name=/^(DateTimeFormat|RelativeTimeFormat)$/]",
          message: "Date formatters belong in lib/format.ts and render through useTimeFormat().",
        },
      ],
      "no-restricted-imports": [
        "error",
        {
          paths: [
            {
              name: "@/lib/format",
              importNames: ["formatDate", "formatDateStable", "formatDateTime", "formatExact", "formatStable"],
              message: "Use useTimeFormat() so the first render is the same on the server and in the browser.",
            },
          ],
        },
      ],
    },
  },
  // Override default ignores of eslint-config-next.
  globalIgnores([
    // Default ignores of eslint-config-next:
    ".next/**",
    "out/**",
    "build/**",
    "next-env.d.ts",
    // Generated from paperboat-server's OpenAPI document.
    "lib/api-schema.d.ts",
    // The browser terminal runtime, copied as built.
    "public/terminal/**",
    // The ENV vault worker and the Go toolchain's wasm_exec.js, copied as built.
    "public/environment-vault/**",
  ]),
])

export default eslintConfig
