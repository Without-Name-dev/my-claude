import { describe, expect, test } from 'bun:test'
import { buildQuestionAnswers } from '../lib/question-answers'
import type { Question } from '../types'

const questions: Question[] = [
  {
    question: '选择部署方式？',
    options: [{ label: '本地' }, { label: '服务器' }],
  },
  {
    question: '需要哪些平台？',
    multiSelect: true,
    options: [{ label: 'Windows' }, { label: 'iOS' }],
  },
]

describe('AskUserQuestion answers', () => {
  test('uses question text as keys and comma-separated strings for multi-select', () => {
    expect(buildQuestionAnswers(questions, { 0: 1, 1: [0, 1] }, {})).toEqual({
      '选择部署方式？': '服务器',
      '需要哪些平台？': 'Windows, iOS',
    })
  })

  test('includes typed answers, even before a separate field action', () => {
    expect(
      buildQuestionAnswers(
        questions,
        { 1: [1] },
        { 0: ' 自定义服务器 ', 1: 'Android' },
      ),
    ).toEqual({
      '选择部署方式？': '自定义服务器',
      '需要哪些平台？': 'iOS, Android',
    })
  })
})
