import { expect, test } from 'bun:test'
import { splitPastedPaths } from '../pastePaths.js'

test('splits quoted Windows file drops without splitting spaces in paths', () => {
  expect(
    splitPastedPaths('"C:\\a folder\\one.png" "D:\\b folder\\two.jpg"'),
  ).toEqual(['"C:\\a folder\\one.png"', '"D:\\b folder\\two.jpg"'])
  expect(splitPastedPaths('"C:/one.png"\r\n"D:/two.png"')).toEqual([
    '"C:/one.png"',
    '"D:/two.png"',
  ])
})
