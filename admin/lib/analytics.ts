/** The index of the funnel step that loses the largest share of the step before it (-1 when nothing is lost). */
export function dropoff(funnel: { count: number }[]) {
  let worst = -1
  let share = 0
  for (let i = 1; i < funnel.length; i++) {
    const lost = funnel[i - 1].count > 0 ? 1 - funnel[i].count / funnel[i - 1].count : 0
    if (lost > share) [worst, share] = [i, lost]
  }
  return worst
}
