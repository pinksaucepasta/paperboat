export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 600 360" fill="currentColor" aria-hidden className={className}>
      <path
        fillRule="evenodd"
        d="M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
      />
    </svg>
  )
}
