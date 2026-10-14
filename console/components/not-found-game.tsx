"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { useEffect, useRef, useState } from "react"
import { Logo } from "@/components/logo"
import { Button } from "@/components/ui/button"

/*
 * The 404: the missing page folded itself into a paper boat and sailed off.
 *
 * Made of the brand bar's materials, at full screen: the logo gradient with its topography, the
 * white-to-pale-blue boat, the same planing motion (the bow lifts with speed and throttle, a smear
 * grows past cruising speed). All the text is there from the first frame; the story plays out under it.
 *
 * The page and the boat are both drawn from square cells, the logo's own pixels. The fold moves
 * every cell of the page to a cell of the boat along a lifted arc; as each one travels it flips
 * over (its width runs through zero and it shows the paper's darker back), and the crease sweeps
 * from the dog-eared corner across the sheet, so it reads as paper being folded, not pixels
 * sliding. "Call it back" runs it in reverse: the boat sails in, unfolds into the page, which
 * now carries the address you asked for, then gives up, folds again and leaves.
 */

// The logo's boat on a 20 × 12 grid (each cell 30 of its 600 × 360 units).
const boatRows: [number, number][][] = [
  [[9, 10]],
  [[9, 10]],
  [[7, 12]],
  [[7, 12]],
  [[5, 14]],
  [[5, 14]],
  [
    [0, 3],
    [16, 19],
  ],
  [
    [0, 3],
    [16, 19],
  ],
  [[2, 17]],
  [[2, 17]],
  [[4, 15]],
  [[4, 15]],
]
const boatCols = 20
const boatRowCount = 12

// A sheet of paper, 12 × 15 cells: a dog-eared top-right corner and a few lines of text.
const pageCols = 12
const pageRowCount = 15
const ear = 3
const textLines: Record<number, number> = { 5: 8, 7: 7, 9: 8, 11: 5 } // row → line length from column 2

type Tone = "paper" | "flap" | "ink"
type Cell = { from: [number, number]; to: [number, number]; tone: Tone; delay: number }

const tones: Record<Tone, string> = { paper: "#ffffff", flap: "#c9d7ff", ink: "#b3c6ff" }
const backOfPaper = "#d6e1ff"

// The fold: each cell takes `cellTime` once the crease reaches it; the crease takes `stagger` to cross.
const cellTime = 0.8
const stagger = 0.6
const foldTime = cellTime + stagger

const cells: Cell[] = (() => {
  const boat: [number, number][] = []
  boatRows.forEach((spans, row) =>
    spans.forEach(([a, b]) => {
      for (let col = a; col <= b; col++) boat.push([col, row])
    }),
  )
  const page: { at: [number, number]; tone: Tone }[] = []
  for (let row = 0; row < pageRowCount; row++)
    for (let col = 0; col < pageCols; col++) {
      const inEar = col >= pageCols - ear && row < ear
      // The dog-ear: the corner is folded down, so its upper-right half is gone and the rest is the flap.
      if (inEar && col - (pageCols - ear) > row) continue
      const line = textLines[row]
      const tone: Tone = inEar ? "flap" : line !== undefined && col >= 2 && col < 2 + line ? "ink" : "paper"
      page.push({ at: [col, row], tone })
    }
  // The top of the sheet becomes the sail and its foot the hull: both in reading order, spread evenly.
  return page.map(({ at, tone }, index) => {
    const to = boat[Math.floor((index * boat.length) / page.length)]
    // The crease sweeps from the dog-eared corner (top right) toward the bottom left.
    const sweep = (pageCols - 1 - at[0] + at[1]) / (pageCols - 1 + pageRowCount - 1)
    return { from: at, to, tone, delay: sweep * stagger }
  })
})()

// Motion, in the brand bar's terms.
const topSpeed = 900 // px/s leaving
const throttle = 650 // px/s² while leaving
const maxTilt = 8 // degrees of bow lift at top speed
const throttleTilt = 4 // extra lift while the throttle is open
const tiltLag = 0.09 // s
const blurFrom = 200 // px/s below which the boat stays sharp

const easeInOut = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - (-2 * t + 2) ** 3 / 2)
const easeOut = (t: number) => 1 - (1 - t) ** 3
const clamp01 = (t: number) => Math.max(0, Math.min(1, t))

type Phase = "page" | "fold" | "afloat" | "sail" | "gone" | "return" | "settle" | "unfold" | "show"
const replies = [
  "It came back empty. Still nothing at this address.",
  "Checked again. Nothing.",
  "It really isn’t here.",
  "The boat would like you to try the machines page.",
]

export function NotFoundGame() {
  const pathname = usePathname()
  const canvas = useRef<HTMLCanvasElement>(null)
  const call = useRef<() => void>(() => {})
  const [reply, setReply] = useState<string>()
  const [away, setAway] = useState(false)
  const lost = safeDecode(pathname || "/")

  useEffect(() => {
    const el = canvas.current
    const context = el?.getContext("2d")
    if (!el || !context) return
    const ctx = context
    const calm = matchMedia("(prefers-reduced-motion: reduce)").matches
    const mono =
      getComputedStyle(document.documentElement).getPropertyValue("--font-jetbrains-mono").trim() || "monospace"

    let width = 0
    let height = 0
    let unit = 8 // px per cell
    let sea = 0 // the swell's resting y
    const fit = () => {
      const ratio = window.devicePixelRatio || 1
      width = el.clientWidth
      height = el.clientHeight
      el.width = Math.round(width * ratio)
      el.height = Math.round(height * ratio)
      ctx.setTransform(ratio, 0, 0, ratio, 0, 0)
      unit = Math.max(6, Math.min(10, Math.round(width / 140)))
      sea = Math.round(height * 0.72)
    }
    fit()
    const observer = new ResizeObserver(fit)
    observer.observe(el)

    let phase: Phase = calm ? "gone" : "page"
    let phaseStart = performance.now()
    let x = width / 2 // boat centre
    let v = 0
    let tilt = 0
    let fold = calm ? 1 : 0 // 0 = page, 1 = boat
    let clock = 0
    let calls = 0
    let last = performance.now()
    let frame = 0
    // Foam left behind the stern: cells that fade as the sea rolls on.
    const wake: { x: number; y: number; life: number }[] = []
    let foamAt = 0

    const go = (next: Phase, now: number) => {
      phase = next
      phaseStart = now
    }

    // The swell: two sines, quantised to half-cells so it stays pixel-made.
    const swell = (px: number) => {
      const y = Math.sin(px * 0.017 + clock * 1.5) * 6 + Math.sin(px * 0.043 - clock * 2.2) * 3
      return Math.round(y / (unit / 2)) * (unit / 2)
    }

    call.current = () => {
      if (phase !== "gone") return
      calls++
      setReply(undefined)
      setAway(false)
      const now = performance.now()
      if (calm) {
        x = width / 2
        fold = 0
        setReply(replies[Math.min(calls - 1, replies.length - 1)])
        go("show", now)
        return
      }
      x = -boatCols * unit
      v = 0
      go("return", now)
    }

    /** Draws the page↔boat at fold progress `k`, centred on `cx`, the hull sitting on `base`. */
    const paint = (cx: number, base: number, k: number, smear: number) => {
      const pageLeft = cx - (pageCols * unit) / 2
      const pageTop = base - pageRowCount * unit - unit
      const boatLeft = cx - (boatCols * unit) / 2
      const boatTop = base - boatRowCount * unit * 0.8 // the hull sits a little into the swell

      const layer = (alpha: number, offset: number) => {
        ctx.globalAlpha = alpha
        for (const cell of cells) {
          // Each cell starts once the crease reaches it, then arcs up and over, flipping as it goes.
          const t = easeInOut(clamp01((k * foldTime - cell.delay) / cellTime))
          const sx = pageLeft + cell.from[0] * unit
          const sy = pageTop + cell.from[1] * unit
          const ex = boatLeft + cell.to[0] * unit
          const ey = boatTop + cell.to[1] * unit
          const lift = Math.sin(Math.PI * t) * unit * 4
          const px = sx + (ex - sx) * t - offset
          const py = sy + (ey - sy) * t - lift
          const turn = Math.cos(Math.PI * t)
          // Resting cells overlap a hair so the rotated boat shows no seams.
          const pad = t <= 0 || t >= 1 ? 0.5 : 0
          const w = Math.max(unit * 0.2, unit * Math.abs(turn))
          ctx.fillStyle =
            t <= 0
              ? tones[cell.tone]
              : t >= 1
                ? cell.to[1] < 6
                  ? "#ffffff"
                  : "#eaf0ff"
                : turn < 0
                  ? backOfPaper
                  : "#ffffff"
          ctx.fillRect(px + (unit - w) / 2 - pad, py - pad, w + pad * 2, unit + pad * 2)
        }
      }
      // At speed, fainter copies trail behind: the brand bar's motion blur, in pixels.
      if (smear > 0) for (let i = 3; i >= 1; i--) layer(0.16 * smear * (1 - i / 4), i * smear * unit * 1.5)
      layer(1, 0)
      ctx.globalAlpha = 1
    }

    const draw = (now: number) => {
      const dt = Math.min(0.05, (now - last) / 1000)
      last = now
      if (!calm) clock += dt
      const elapsed = (now - phaseStart) / 1000
      const lastV = v

      let smear = 0
      if (phase === "page") {
        x = width / 2
        if (elapsed > 1.8) go("fold", now)
      } else if (phase === "fold") {
        fold = clamp01(elapsed / foldTime)
        if (fold >= 1) go("afloat", now)
      } else if (phase === "afloat") {
        if (elapsed > 0.8) go("sail", now)
      } else if (phase === "sail") {
        v = Math.min(topSpeed, v + throttle * dt)
        x += v * dt
        if (x - (boatCols * unit) / 2 > width + 40) {
          v = 0
          setAway(true)
          go("gone", now)
        }
      } else if (phase === "return") {
        // Sails in fast and eases to a stop mid-screen.
        const k = clamp01(elapsed / 1.5)
        const from = -boatCols * unit
        const next = from + (width / 2 - from) * easeOut(k)
        v = (next - x) / Math.max(dt, 0.001)
        x = next
        if (k >= 1) {
          v = 0
          go("settle", now)
        }
      } else if (phase === "settle") {
        if (elapsed > 0.5) go("unfold", now)
      } else if (phase === "unfold") {
        fold = 1 - clamp01(elapsed / foldTime)
        if (fold <= 0) {
          setReply(replies[Math.min(calls - 1, replies.length - 1)])
          go("show", now)
        }
      } else if (phase === "show") {
        if (!calm && elapsed > 2.6) go("fold", now)
      }
      if (phase !== "sail" && phase !== "return") v = 0
      smear = clamp01((v - blurFrom) / (topSpeed - blurFrom))

      // Planing: the bow lifts with speed and with an open throttle, dips a little when slowing.
      const accel = (v - lastV) / Math.max(dt, 0.001)
      const open = Math.max(-1, Math.min(1, accel / 1500))
      const target = -maxTilt * Math.min(1, v / topSpeed) ** 1.2 - throttleTilt * (open < 0 ? open * 0.3 : open)
      tilt += (target - tilt) * (1 - Math.exp(-dt / tiltLag))

      ctx.clearRect(0, 0, width, height)

      // Foam behind a moving boat.
      if (v > 60 && fold >= 1) {
        foamAt += v * dt
        while (foamAt > unit) {
          foamAt -= unit
          const stern = x - (boatCols * unit) / 2 + unit * 2
          wake.push({ x: stern, y: sea + swell(stern) - unit * 0.5, life: 1 })
        }
      }
      for (let i = wake.length - 1; i >= 0; i--) {
        const foam = wake[i]
        foam.life -= dt * 0.9
        foam.x -= dt * 24
        if (foam.life <= 0) {
          wake.splice(i, 1)
          continue
        }
        ctx.fillStyle = `rgba(255,255,255,${0.7 * foam.life})`
        const size = Math.max(2, Math.round((unit / 2) * (0.5 + foam.life)))
        ctx.fillRect(Math.round(foam.x), Math.round(foam.y + (1 - foam.life) * unit), size, size)
      }

      // The swell: a near row and a fainter far one, broken into dashes that roll left.
      for (const [row, alpha, shift] of [
        [0, 0.55, 0],
        [1, 0.2, 37],
      ] as const) {
        ctx.fillStyle = `rgba(255,255,255,${alpha})`
        for (let px = 0; px < width + unit; px += unit) {
          if ((Math.floor((px + clock * 30) / unit) + row) % 3 === 0) continue
          ctx.fillRect(px, sea + row * unit * 2.2 + swell(px + shift), unit, unit / 2)
        }
      }

      if (phase !== "gone") {
        const base = sea + swell(x)
        const bob = fold >= 1 && v === 0 ? Math.sin(clock * 2) * 0.025 : 0
        const pivotX = x - (boatCols * unit) / 2 + unit * 3 // near the stern, low on the hull
        ctx.save()
        ctx.translate(pivotX, base)
        ctx.rotate((tilt * Math.PI) / 180 + bob)
        ctx.translate(-pivotX, -base)
        paint(x, base, fold, smear)
        ctx.restore()

        // While it's a page, it's labelled with the address you asked for; the label fades as it folds.
        const label = 1 - clamp01(fold * 4)
        if (label > 0) {
          ctx.font = `500 ${Math.max(12, Math.round(unit * 1.4))}px ${mono}`
          ctx.textAlign = "center"
          ctx.fillStyle = `rgba(255,255,255,${0.8 * label})`
          ctx.fillText(lost.length > 40 ? `${lost.slice(0, 39)}…` : lost, x, base + unit * 6)
        }
      }

      frame = requestAnimationFrame(draw)
    }
    frame = requestAnimationFrame((now) => {
      // With reduced motion there's no voyage: the boat is already gone.
      if (calm) setAway(true)
      draw(now)
    })

    return () => {
      cancelAnimationFrame(frame)
      observer.disconnect()
    }
  }, [lost])

  return (
    <main className="brand-bar relative h-svh min-h-[34rem] w-full overflow-hidden text-white select-none">
      {/* .brand-bar makes its children position: relative (for its texture), so the layers live in one. */}
      <div className="h-full w-full">
        <canvas ref={canvas} className="absolute inset-0 h-full w-full" aria-hidden />

        <Link
          href="/"
          aria-label="Paperboat home"
          className="absolute top-6 left-6 flex items-center gap-3 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-white/50"
        >
          <Logo className="h-[18px]" />
          <span className="font-heading text-[18px] leading-none font-medium tracking-[-0.025em]">paperboat</span>
        </Link>

        <div className="absolute inset-x-0 top-[18vh] mx-auto flex max-w-4xl flex-col items-center gap-4 px-6 text-center">
          <p className="text-sm font-medium text-white/85">
            <span className="mr-2 font-mono text-white/60">404</span>Page not found
          </p>
          <h1 className="font-heading text-[clamp(2rem,5vw,3.5rem)] leading-[1.05] font-semibold tracking-[-0.03em] text-balance">
            This page folded itself into a boat
          </h1>
          <p className="min-h-[1.5em] text-base text-white/75" aria-live="polite">
            {reply ?? "and sailed off. There’s nothing at this address."}
          </p>
          <div className="mt-3 flex flex-wrap justify-center gap-2">
            <Button
              size="lg"
              className="bg-white text-[#0f3dd8] hover:bg-white/90"
              render={<Link href="/" />}
              nativeButton={false}
            >
              Back to machines
            </Button>
            <Button
              size="lg"
              variant="secondary"
              className="bg-white/15 text-white hover:bg-white/25"
              disabled={!away}
              onClick={() => call.current()}
            >
              Call it back
            </Button>
          </div>
        </div>
      </div>
    </main>
  )
}

function safeDecode(path: string) {
  try {
    return decodeURIComponent(path)
  } catch {
    return path
  }
}
