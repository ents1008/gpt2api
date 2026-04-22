export interface BrandParts {
  brand: string
  qq: string
  qqLabel: string
  sep: string
}

const BRAND: BrandParts = {
  brand: '积云',
  qq: '601070651',
  qqLabel: '交流QQ群 ',
  sep: ' · ',
}

export function brandParts(): BrandParts {
  return BRAND
}

export function brandPlainText(): string {
  const p = brandParts()
  return [p.brand, `${p.qqLabel}${p.qq}`].join(p.sep)
}

let _warned = false
export function printBrandToConsole(): void {
  if (_warned) return
  _warned = true
  try {
    const p = brandParts()
    console.log(
      `%c${p.brand}%c  ${p.qqLabel}${p.qq}`,
      'font-weight:700;color:#409eff;font-size:13px;',
      'color:#909399;font-size:12px;',
    )
  } catch {
    /* ignore */
  }
}

export function startBrandGuard(): void {
  // 当前品牌信息已经直接渲染在布局中,这里保留空实现避免重复注入额外内容。
}
