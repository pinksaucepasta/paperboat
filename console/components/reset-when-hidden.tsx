"use client"

import { Fragment, useLayoutEffect, useState } from "react"

/**
 * One rule for every flow (dialog, wizard, sheet, draft): leaving a page ends it.
 * Next keeps visited pages mounted but hidden (cacheComponents), so their open dialogs, steps and
 * drafts would come back on return. Hiding runs this layout-effect cleanup, which remounts the
 * subtree fresh while it is hidden. Flows that must survive a reload or a share live in the URL.
 * Used by each route directory's template.tsx.
 */
export function ResetWhenHidden({ children }: { children: React.ReactNode }) {
  const [visit, setVisit] = useState(0)
  useLayoutEffect(() => () => setVisit((n) => n + 1), [])
  return <Fragment key={visit}>{children}</Fragment>
}
