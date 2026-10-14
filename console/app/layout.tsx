import type { Metadata } from "next"
import { Figtree, Geist, JetBrains_Mono } from "next/font/google"
import { cn } from "@/lib/utils"
import { Providers } from "@/components/providers"
import "./globals.css"

const sans = Geist({ subsets: ["latin"], variable: "--font-sans" })
const mono = JetBrains_Mono({ subsets: ["latin"], variable: "--font-jetbrains-mono" })
const heading = Figtree({ subsets: ["latin"], variable: "--font-heading" })

export const metadata: Metadata = {
  title: { default: "Paperboat", template: "%s · Paperboat" },
}

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html
      lang="en"
      suppressHydrationWarning
      className={cn("h-full antialiased", sans.variable, mono.variable, heading.variable)}
    >
      <body className="min-h-full">
        <Providers>{children}</Providers>
      </body>
    </html>
  )
}
