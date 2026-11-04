// React Query never polls where there is no window (a server render), so tests that count polls need one.
;(globalThis as { window?: unknown }).window ??= globalThis
