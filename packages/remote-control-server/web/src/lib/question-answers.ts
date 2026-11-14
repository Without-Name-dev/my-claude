import type { Question } from '../types'

/** Match AskUserQuestion's CLI contract: question text -> answer string. */
export function buildQuestionAnswers(
  questions: Question[],
  selections: Record<string, number | number[]>,
  otherTexts: Record<string, string>,
): Record<string, string> {
  const answers: Record<string, string> = {}
  questions.forEach((question, index) => {
    if (!question.question) return
    const selected = selections[index]
    const indices = Array.isArray(selected)
      ? selected
      : typeof selected === 'number'
        ? [selected]
        : []
    const labels = indices.flatMap(i => {
      const label = question.options?.[i]?.label
      return label ? [label] : []
    })
    const other = otherTexts[index]?.trim()
    const answer = question.multiSelect
      ? [...labels, ...(other ? [other] : [])].join(', ')
      : other || labels[0] || ''
    if (answer) answers[question.question] = answer
  })
  return answers
}
