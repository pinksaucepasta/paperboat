import { defineConfig, globalIgnores } from "eslint/config"
import nextVitals from "eslint-config-next/core-web-vitals"
import nextTs from "eslint-config-next/typescript"

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
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
