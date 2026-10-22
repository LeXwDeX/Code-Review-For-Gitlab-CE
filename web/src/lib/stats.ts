import { formatDuration, formatNumber, readNumber } from './format'
import type { Stats } from '../api/types'

export function statNumber(stats: Stats | undefined, key: string): number | null {
  if (!stats) return null
  const value = stats[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

export function statDisplay(stats: Stats | undefined, key: string): string {
  const value = statNumber(stats, key)
  return value === null ? '不可用' : formatNumber(value)
}

export function formatRatio(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0 || value > 1) return '不可用'
  return `${(value * 100).toFixed(1)}%`
}

export function formatMs(value: number | null): string {
  return value === null ? '不可用' : formatDuration(value)
}

export function statsMetricNote(stats: Stats | undefined, pieces: Array<{ key: string; label: string; kind?: 'number' | 'ratio' | 'duration' }>): string {
  return pieces.map((piece) => {
    const value = readNumber(stats, [piece.key])
    const formatted = value === undefined
      ? '不可用'
      : piece.kind === 'ratio'
        ? formatRatio(value)
        : piece.kind === 'duration'
          ? formatDuration(value)
          : formatNumber(value)
    return `${piece.label} ${formatted}`
  }).join(' · ')
}
