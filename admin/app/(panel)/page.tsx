"use client"

import { useRouter } from "next/navigation"
import { useEffect } from "react"
import { operatorQuery, useApiQuery, type Operator } from "@/lib/api"
import { sectionsFor } from "@/lib/sections"

/** The panel opens on the first page this role may read. */
export default function Home() {
  const router = useRouter()
  const me = useApiQuery<Operator>(operatorQuery.key, operatorQuery.path)
  useEffect(() => {
    const first = me.data ? sectionsFor(me.data.role)[0] : undefined
    if (first) router.replace(first.href)
  }, [me.data, router])
  return null
}
