/**
 * The /security page: four sections, each to be written. TODO-COPY marks every placeholder.
 */
const todo = (what: string) => `TODO-COPY: ${what}`

export const securitySections = [
  { id: "encrypted", title: "What is end-to-end encrypted", text: todo("which traffic and data only your machines can read") },
  { id: "visible", title: "What our servers can see", text: todo("what the control plane and the edges can and cannot see") },
  { id: "storage", title: "Where data lives", text: todo("where each kind of data is stored, and for how long") },
  { id: "report", title: "Reporting a vulnerability", text: todo("how to report, who reads it, and what to expect") },
]
