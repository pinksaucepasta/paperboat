import Link from "next/link"
import { Logo } from "@/components/logo"

/** Full-screen brand page (CLI approval, sign-in failed): the brand-bar gradient, logo top left. */
export function BrandScreen({ children }: { children: React.ReactNode }) {
  return (
    <main className="brand-bar min-h-svh text-white">
      {/* .brand-bar makes its children position: relative (for its texture), so the layers live in one. */}
      <div className="flex min-h-svh flex-col items-center justify-center px-6 py-24">
        <Link
          href="/"
          aria-label="Paperboat home"
          className="absolute top-6 left-6 flex items-center gap-3 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-white/50"
        >
          <Logo className="h-[18px]" />
          <span className="font-heading text-[18px] leading-none font-medium tracking-[-0.025em]">paperboat</span>
        </Link>
        <div className="flex w-full max-w-md flex-col gap-6">{children}</div>
      </div>
    </main>
  )
}

/** The white primary button on a brand screen. */
export const brandPrimary = "bg-white text-[#0f3dd8] hover:bg-white/90"
/** The translucent secondary button on a brand screen. */
export const brandSecondary = "bg-white/15 text-white hover:bg-white/25"

/** The title and line of a brand screen. */
export function Heading({ title, text }: { title: string; text: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <h1 className="font-heading text-[clamp(1.75rem,4vw,2.5rem)] leading-[1.1] font-semibold tracking-[-0.03em] text-balance">
        {title}
      </h1>
      <p className="text-base text-white/75">{text}</p>
    </div>
  )
}

/** A finished request: an icon, what happened and what to do next. */
export function Outcome({ icon, title, children }: { icon: React.ReactNode; title: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 text-center">
      {icon}
      <h1 className="font-heading text-2xl font-semibold tracking-[-0.02em]">{title}</h1>
      <p className="text-base text-white/75">{children}</p>
    </div>
  )
}
