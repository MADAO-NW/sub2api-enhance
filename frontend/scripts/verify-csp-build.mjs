import { readdir, readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

// 校验生产脚本不依赖严格 CSP 禁止的动态代码执行能力。
const assetsDirectory = fileURLToPath(new URL('../../backend/internal/web/dist/assets/', import.meta.url))
const scripts = (await readdir(assetsDirectory)).filter((name) => name.endsWith('.js'))
const forbidden = [
  { label: 'new Function', pattern: /\bnew\s+Function\s*\(/ },
  { label: 'eval', pattern: /\beval\s*\(/ },
]

for (const script of scripts) {
  const source = await readFile(new URL(script, `file://${assetsDirectory}/`), 'utf8')
  for (const rule of forbidden) {
    if (rule.pattern.test(source)) {
      throw new Error(`${script} 包含严格 CSP 禁止的 ${rule.label}`)
    }
  }
}

console.log(`CSP build check passed (${scripts.length} scripts)`)
