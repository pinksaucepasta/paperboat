import type { Metadata } from "next"
import { NotFoundGame } from "@/components/not-found-game"

export const metadata: Metadata = { title: "Page not found" }

/** Any page that doesn’t exist: it folds itself into a paper boat and sails off (components/not-found-game.tsx). */
export default function NotFound() {
  return <NotFoundGame />
}
