/**
 * Display-side renderer for plan markdown (docs/plans/format.md). Best-effort
 * presentation only — the server's parser (api/internal/plans) is the
 * authority; this exists so reviewers read the same structure the pipeline
 * executes: frontmatter key grid + numbered steps with judge/uses/on-failure
 * fields.
 */
export interface PlanStep {
  number: number
  intent: string
  judge: string | null
  uses: string | null
  onFailure: string | null
}

export interface PlanView {
  frontmatter: [string, string][]
  steps: PlanStep[]
}

function frontmatterValue(value: unknown): string {
  if (value == null) return '—'
  if (Array.isArray(value)) return value.map((item) => String(item)).join(', ')
  if (typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>)
      .map(([key, inner]) => `${key}: ${frontmatterValue(inner)}`)
      .join('; ')
  }
  return String(value)
}

export function renderPlanView(content: string, frontmatter: Record<string, unknown>): PlanView {
  const keys = Object.keys(frontmatter)
  const fm: [string, string][] = keys.map((key) => [key, frontmatterValue(frontmatter[key])])

  const steps: PlanStep[] = []
  let current: PlanStep | null = null

  for (const rawLine of content.split('\n')) {
    const line = rawLine.trimEnd()
    const stepMatch = /^(\d+)\.\s+(.*)$/.exec(line.trim())
    if (stepMatch) {
      current = {
        number: Number(stepMatch[1]),
        intent: stepMatch[2],
        judge: null,
        uses: null,
        onFailure: null,
      }
      steps.push(current)
      continue
    }
    if (!current) continue

    const trimmed = line.trim()
    const fieldMatch = /^(judge|uses|on-failure):\s*(.*)$/.exec(trimmed)
    if (fieldMatch) {
      const field = fieldMatch[1]
      if (field === 'judge') current.judge = fieldMatch[2]
      else if (field === 'uses') current.uses = fieldMatch[2]
      else current.onFailure = fieldMatch[2]
    } else if (trimmed !== '' && !trimmed.startsWith('#')) {
      current.intent = `${current.intent} ${trimmed}`.trim()
    }
  }

  return { frontmatter: fm, steps }
}
