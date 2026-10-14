"use client"

import Link from "next/link"
import { useCallback, useEffect, useId, useRef } from "react"

/**
 * The sidebar's brand bar. On load (and on click) the boat glides in and slows as it reaches the
 * name, which fades in behind it while it crosses the word at a calm, steady pace; past it it picks up speed
 * and runs out to the right, then sweeps back in from the left and eases into its berth.
 *
 * While moving it planes like a speedboat: the bow lifts with speed, pivoting near the stern.
 *
 * One clock drives everything. The boat's path is simulated from a speed profile (so speeds
 * join without jumps), and each frame sets the name's reveal edge and the boat's motion blur
 * from that same position and speed, so nothing can drift out of step.
 */
const cruise = 85 // px/s while crossing the name: slow enough to read the reveal
const approach = 450 // px/s entering from off-screen, slowing to cruise before the name
const settle = 380 // ms to slow from approach to cruise
const topSpeed = 900 // px/s once past the name
const speedUp = 650 // ms to accelerate from cruise to top speed
const pause = 180 // ms out of sight before coming back
const back = 950 // ms to sweep back in and settle
const feather = 28 // px of soft edge on the reveal, trailing the boat
const blurFrom = 160 // px/s below which the boat stays sharp (it is crisp while cruising)
const blurPerSpeed = 1 / 140 // px of horizontal blur per px/s above that
const maxBlur = 6 // px
const maxTilt = 8 // degrees of bow lift at top speed (planing)
const tiltLag = 90 // ms: the tilt trails speed changes, as a hull takes time to lift and settle
const throttleTilt = 5 // extra degrees of bow lift at full acceleration (the bow rises as the throttle opens)
const fullThrottle = 2000 // px/s² counted as full acceleration
const brakeShare = 0.3 // slowing down dips the bow only this share as much as throttle lifts it

const smoothstep = (t: number) => (t <= 0 ? 0 : t >= 1 ? 1 : t * t * (3 - 2 * t))
/** Fast start, long soft stop; its starting speed is 3 × distance / duration. */
const easeOutCubic = (t: number) => 1 - (1 - t) ** 3

type Sample = { t: number; x: number; v: number }

/** The outbound leg as time-stamped samples: arrive, slow to cruise over the name, speed up. */
function outbound(from: number, to: number, cruiseUntil: number): Sample[] {
  const samples: Sample[] = [{ t: 0, x: from, v: approach }]
  const dt = 1000 / 240
  let t = 0
  let x = from
  let accelerateAt = Infinity
  while (x < to) {
    t += dt
    if (accelerateAt === Infinity && x >= cruiseUntil) accelerateAt = t
    const v =
      t < accelerateAt
        ? cruise + (approach - cruise) * (1 - smoothstep(t / settle))
        : cruise + (topSpeed - cruise) * smoothstep((t - accelerateAt) / speedUp)
    x += (v * dt) / 1000
    samples.push({ t, x, v })
  }
  return samples
}

function sampleAt(samples: Sample[], t: number): Sample {
  let low = 0
  let high = samples.length - 1
  while (high - low > 1) {
    const mid = (low + high) >> 1
    if (samples[mid].t <= t) low = mid
    else high = mid
  }
  const a = samples[low]
  const b = samples[high]
  const k = b.t === a.t ? 0 : Math.min(1, Math.max(0, (t - a.t) / (b.t - a.t)))
  return { t, x: a.x + (b.x - a.x) * k, v: a.v + (b.v - a.v) * k }
}

export function BrandBar({ onNavigate }: { onNavigate?: () => void }) {
  const link = useRef<HTMLAnchorElement>(null)
  const boat = useRef<SVGSVGElement>(null)
  const blur = useRef<SVGFEGaussianBlurElement>(null)
  const name = useRef<HTMLSpanElement>(null)
  const frame = useRef(0)

  const play = useCallback(() => {
    const container = link.current
    const boatEl = boat.current
    const blurEl = blur.current
    const nameEl = name.current
    if (!container || !boatEl || !blurEl || !nameEl) return
    cancelAnimationFrame(frame.current)

    const setBlur = (speed: number) => {
      const px = Math.min(maxBlur, Math.max(0, Math.abs(speed) - blurFrom) * blurPerSpeed)
      // The filter works in the SVG's own units: convert from screen pixels.
      const units = (px * 600) / boatEl.getBoundingClientRect().width
      blurEl.setAttribute("stdDeviation", `${units.toFixed(2)} 0`)
    }
    const finish = () => {
      boatEl.style.transform = "none"
      nameEl.style.maskImage = "none"
      setBlur(0)
    }
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) return finish()

    // Positions are relative to the boat's berth, which is the link's left edge.
    const width = container.clientWidth
    const boatWidth = boatEl.getBoundingClientRect().width
    const nameLeft = nameEl.offsetLeft
    const nameRight = nameLeft + nameEl.offsetWidth
    const offLeft = -(boatWidth + 24)
    const offRight = width + 24
    // Cruise until the boat's middle has passed the end of the name, then speed up.
    const path = outbound(offLeft, offRight, nameRight - boatWidth / 2 + 6)
    const out = path[path.length - 1].t

    const setReveal = (edge: number) => {
      const local = edge - nameLeft
      nameEl.style.maskImage = `linear-gradient(to right, #000 ${local - feather}px, transparent ${local}px)`
    }

    // Bow lift grows a little faster than speed (nearly level while cruising, clearly lifted when
    // fast), plus a throttle term: accelerating lifts the bow at once, slowing dips it slightly.
    const tiltFor = (speed: number, accel: number) => {
      const throttle = Math.max(-1, Math.min(1, accel / fullThrottle))
      return (
        -maxTilt * Math.min(1, Math.abs(speed) / topSpeed) ** 1.2 -
        throttleTilt * (throttle < 0 ? throttle * brakeShare : throttle)
      )
    }
    // It arrives already moving, so it starts with that speed's lift rather than level.
    let tilt = tiltFor(path[0].v, 0)
    let last = performance.now()
    let lastSpeed = path[0].v
    let lastPhase = 0

    const start = performance.now()
    const tick = (now: number) => {
      const elapsed = now - start
      let x: number
      let v: number
      const phase = elapsed < out ? 0 : elapsed < out + pause ? 1 : 2
      if (elapsed < out) {
        ;({ x, v } = sampleAt(path, elapsed))
        // The name appears in the boat's wake, up to the boat's middle.
        setReveal(x + boatWidth / 2)
      } else if (elapsed < out + pause) {
        x = offRight
        v = 0
        nameEl.style.maskImage = "none"
      } else if (elapsed < out + pause + back) {
        const k = (elapsed - out - pause) / back
        x = offLeft * (1 - easeOutCubic(k))
        v = ((-offLeft * 3 * (1 - k) ** 2) / back) * 1000
      } else {
        return finish()
      }
      // Speed jumps where legs meet (out of sight); start each leg from rest, not from a spike.
      if (phase !== lastPhase) {
        lastPhase = phase
        lastSpeed = v
        tilt = phase === 2 ? tiltFor(v, 0) : 0
      }
      // Exponential smoothing toward the target, independent of frame rate.
      const dt = Math.max(1, now - last)
      const accel = ((v - lastSpeed) / dt) * 1000
      tilt += (tiltFor(v, accel) - tilt) * (1 - Math.exp(-dt / tiltLag))
      last = now
      lastSpeed = v
      boatEl.style.transform = `translateX(${x.toFixed(2)}px) rotate(${tilt.toFixed(2)}deg)`
      setBlur(v)
      frame.current = requestAnimationFrame(tick)
    }
    boatEl.style.transform = `translateX(${offLeft}px)`
    setReveal(offLeft)
    frame.current = requestAnimationFrame(tick)
  }, [])

  useEffect(() => {
    play()
    return () => cancelAnimationFrame(frame.current)
  }, [play])

  return (
    // The whole blue bar is the link: the stretched ::after covers its padding (the bar clips it),
    // without moving the link's box, which the animation measures from.
    <Link
      ref={link}
      href="/"
      data-brand-bar
      className="relative flex w-full items-center gap-3 outline-none after:absolute after:-inset-x-4 after:-inset-y-8 focus-visible:ring-2 focus-visible:ring-white/50"
      onClick={() => {
        play()
        onNavigate?.()
      }}
    >
      {/* Server-rendered at the start line (boat off to the left, name hidden) so nothing
          flashes before the first frame. */}
      <BoatMark ref={boat} blurRef={blur} style={{ transform: "translateX(-56px)" }} />
      {/* The reveal mask covers only this box, and leading-none puts the descenders (the p's)
          outside it; the padding gives them room inside it and the negative margin keeps the layout. */}
      <span
        ref={name}
        className="-my-[0.3em] py-[0.3em] font-heading text-[18px] leading-none font-medium tracking-[-0.025em]"
        style={{ maskImage: "linear-gradient(transparent, transparent)" }}
      >
        paperboat
      </span>
    </Link>
  )
}

/** The logo's boat with the app icon's fill (white to pale blue) and a horizontal motion blur. */
function BoatMark({
  ref,
  blurRef,
  style,
}: {
  ref: React.Ref<SVGSVGElement>
  blurRef: React.Ref<SVGFEGaussianBlurElement>
  style?: React.CSSProperties
}) {
  const id = useId()
  return (
    <svg
      ref={ref}
      style={style}
      viewBox="0 0 600 360"
      aria-hidden
      overflow="visible"
      className="relative z-10 h-[18px] shrink-0 origin-[25%_90%] drop-shadow-[0_1px_1px_rgb(0_0_40/0.25)] will-change-transform"
    >
      <defs>
        <linearGradient id={`${id}-fill`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="#fff" />
          <stop offset="1" stopColor="#eaf0ff" />
        </linearGradient>
        {/* Wide region so the blur is not clipped at the boat's edges. */}
        <filter id={`${id}-blur`} x="-50%" y="0" width="200%" height="100%">
          <feGaussianBlur ref={blurRef} stdDeviation="0 0" />
        </filter>
      </defs>
      <path
        fill={`url(#${id}-fill)`}
        filter={`url(#${id}-blur)`}
        fillRule="evenodd"
        d="M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
      />
    </svg>
  )
}
