/** Structured data shared by pages (schema.org). Plain text only: answers with HTML are stripped. */
import type { Question } from "./components/Faq.astro"

const text = (html: string) => html.replace(/<[^>]+>/g, "")

export const faqSchema = (items: Question[]) => ({
  "@type": "FAQPage",
  mainEntity: items.map((item) => ({
    "@type": "Question",
    name: item.q,
    acceptedAnswer: { "@type": "Answer", text: text(item.a) },
  })),
})

/** `trail` is the path from the home page: [["Features", "/features/"], ["Terminal", "/features/terminal/"]]. */
export const breadcrumbSchema = (site: URL | undefined, trail: [string, string][]) => ({
  "@type": "BreadcrumbList",
  itemListElement: [["Paperboat", "/"] as [string, string], ...trail].map(([name, path], index) => ({
    "@type": "ListItem",
    position: index + 1,
    name,
    item: new URL(path, site).href,
  })),
})

export const softwareSchema = (site: URL | undefined, description: string) => ({
  "@type": "SoftwareApplication",
  name: "Paperboat",
  applicationCategory: "DeveloperApplication",
  operatingSystem: "Linux, macOS",
  description,
  url: new URL("/", site).href,
  offers: { "@type": "Offer", price: "0", priceCurrency: "USD" },
})
