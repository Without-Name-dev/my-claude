import {
  getCommands,
  isBridgeSafeCommand,
  getBridgeCommandSafety,
} from '../src/commands.ts'
const cmds = await getCommands(process.cwd())
console.log('total', cmds.length)
const safe = cmds.filter(isBridgeSafeCommand)
console.log('safe', safe.length)
for (const c of safe) {
  const s = getBridgeCommandSafety(c, '')
  const hint = (c as any).argumentHint || ''
  console.log(
    `${c.name}\t${c.type}\t${s.ok ? 'OK' : 'BLOCK:' + (s as any).reason}\t${hint}`,
  )
}
console.log('--- blocked (first 40) ---')
for (const c of cmds.filter(c => !isBridgeSafeCommand(c)).slice(0, 40)) {
  console.log(`${c.name}\t${c.type}`)
}
