/**
 * The ENV variable-name rule of shared/envname (Go): ASCII letters, digits and underscores, not starting with a
 * digit, 1 to 128 bytes (the same as characters here), and not a name that changes how a process starts. `names.test.ts` checks it against
 * the list the Go rule is tested with, so the two cannot drift. The vault engine enforces it again on every write.
 */
const reservedPrefixes = ["PAPERBOAT_", "LD_", "DYLD_"]
const reservedNames = ["NODE_OPTIONS", "PYTHONPATH", "PYTHONHOME", "GOTRACEBACK"]

export function validVariableName(name: string): boolean {
  if (!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(name)) return false
  const upper = name.toUpperCase()
  return !reservedPrefixes.some((prefix) => upper.startsWith(prefix)) && !reservedNames.includes(upper)
}
