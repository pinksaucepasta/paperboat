/**
 * The /security page: four sections, each to be written.
 */
export const securitySections = [
  { id: "encrypted", title: "What is end-to-end encrypted", text: "Terminals, SSH, private access, file transfers and ENV values are encrypted between your devices with keys that never leave them. Our servers pass the bytes along but can’t read them." },
  { id: "visible", title: "What our servers can see", text: "Our servers see account details, machine names and online state, and how much traffic you use. Public previews and tunnels are the exception to end-to-end: their HTTPS ends at our edge so anyone with the link can open them." },
  { id: "storage", title: "Where data lives", text: "Account and machine records are stored on our servers. Terminal history and Inbox files stay on your machines. ENV values are stored only as ciphertext." },
  { id: "report", title: "Reporting a vulnerability", text: "Report it privately to our security address, which is published here before launch. A person reads every report and keeps you updated until it’s fixed." },
]
