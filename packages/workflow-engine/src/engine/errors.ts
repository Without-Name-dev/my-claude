/** Engine-level expected errors (script errors, caps, nesting). */
export class WorkflowError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'WorkflowError'
  }
}

/**
 * An abort was requested. `scope` distinguishes the two very different cases:
 * a run-level kill must unwind the whole script, while a single-agent kill
 * (`x` in /workflows) may only turn that agent dead — treating it as a run
 * abort killed every sibling agent and the run itself.
 */
export class WorkflowAbortedError extends Error {
  readonly scope: 'run' | 'agent'

  constructor(scope: 'run' | 'agent' = 'run') {
    super(
      scope === 'agent'
        ? 'agent has been aborted'
        : 'workflow has been aborted',
    )
    this.name = 'WorkflowAbortedError'
    this.scope = scope
  }
}
