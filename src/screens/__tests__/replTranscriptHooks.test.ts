import { expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import ts from 'typescript'

/** Hooks after the transcript return disappear when Ctrl+O switches screens. */
function hooksAfterTranscriptReturn(source: string): string[] {
  const file = ts.createSourceFile(
    'REPL.tsx',
    source,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX,
  )
  const repl = file.statements.find(
    (statement): statement is ts.FunctionDeclaration =>
      ts.isFunctionDeclaration(statement) && statement.name?.text === 'REPL',
  )
  if (!repl?.body) throw new Error('REPL function not found')
  const statements = repl.body.statements
  const branch = statements.findIndex(
    statement =>
      ts.isIfStatement(statement) &&
      /screen\s*===\s*['"]transcript['"]/.test(
        statement.expression.getText(file),
      ),
  )
  if (branch < 0) throw new Error('transcript return not found')
  const hooks: string[] = []
  const visit = (node: ts.Node): void => {
    // Callbacks and JSX event handlers do not execute as part of this render.
    if (ts.isFunctionLike(node)) return
    if (ts.isCallExpression(node)) {
      const expression = node.expression
      const name = ts.isIdentifier(expression)
        ? expression.text
        : ts.isPropertyAccessExpression(expression) &&
            ts.isIdentifier(expression.expression) &&
            expression.expression.text === 'React'
          ? expression.name.text
          : ''
      if (/^use[A-Z]/.test(name)) hooks.push(name)
    }
    ts.forEachChild(node, visit)
  }
  for (const statement of statements.slice(branch + 1)) visit(statement)
  return hooks
}

test('the regression guard detects a hook after the transcript return', () => {
  expect(
    hooksAfterTranscriptReturn(`function REPL() {
    React.useRef(0);
    if (screen === 'transcript') return null;
    React.useEffect(() => {}, []);
    return null;
  }`),
  ).toEqual(['useEffect'])
})

test('Ctrl+O does not skip REPL hooks', () => {
  const source = readFileSync(join(import.meta.dir, '..', 'REPL.tsx'), 'utf8')
  expect(hooksAfterTranscriptReturn(source)).toEqual([])
})
