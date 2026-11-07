<script setup lang="ts">
import { computed } from 'vue'

// TrendSeries 描述一条折线：key 对应点在 values 中的键。
export interface TrendSeries { key: string; name: string; color: string }
// TrendPoint 是一个时间点的多序列取值。
export interface TrendPoint { label: string; values: Record<string, number> }

const props = withDefaults(defineProps<{ points: TrendPoint[]; series: TrendSeries[]; height?: number }>(), { height: 200 })
const width = 760
const padding = { top: 12, right: 12, bottom: 24, left: 48 }
const plotWidth = computed(() => width - padding.left - padding.right)
const plotHeight = computed(() => props.height - padding.top - padding.bottom)
const max = computed(() => Math.max(1, ...props.points.flatMap(point => props.series.map(series => point.values[series.key] ?? 0))))
const ticks = computed(() => [0, 0.5, 1].map(ratio => ({ ratio, value: Math.round(max.value * ratio) })))
const xLabels = computed(() => {
  const items = props.points
  if (!items.length) return []
  const middle = Math.floor((items.length - 1) / 2)
  const labels = [{ index: 0, label: items[0]!.label }]
  if (middle > 0 && middle < items.length - 1) labels.push({ index: middle, label: items[middle]!.label })
  if (items.length > 1) labels.push({ index: items.length - 1, label: items[items.length - 1]!.label })
  return labels
})
function x(index: number): number {
  return props.points.length <= 1 ? padding.left + plotWidth.value / 2 : padding.left + (index / (props.points.length - 1)) * plotWidth.value
}
function y(value: number): number {
  return padding.top + plotHeight.value - (value / max.value) * plotHeight.value
}
function polyline(key: string): string {
  return props.points.map((point, index) => `${x(index).toFixed(1)},${y(point.values[key] ?? 0).toFixed(1)}`).join(' ')
}
</script>

<template>
  <div class="space-y-3" data-test="trend-chart">
    <div class="flex flex-wrap gap-4 text-xs">
      <span v-for="series in series" :key="series.key" class="flex items-center gap-2">
        <span class="inline-block h-0.5 w-5" :style="{ backgroundColor: series.color }" />{{ series.name }}
      </span>
    </div>
    <svg class="w-full" :viewBox="`0 0 ${width} ${props.height}`" role="img" :aria-label="series.map(item => item.name).join(' / ')">
      <g>
        <line v-for="tick in ticks" :key="tick.ratio" :x1="padding.left" :x2="width - padding.right" :y1="y(tick.value)" :y2="y(tick.value)"
          stroke="currentColor" stroke-opacity="0.15" stroke-dasharray="4 4" />
        <text v-for="tick in ticks" :key="`label-${tick.ratio}`" :x="padding.left - 8" :y="y(tick.value) + 4" text-anchor="end"
          class="fill-current text-[10px] opacity-60">{{ tick.value }}</text>
      </g>
      <polyline v-for="series in series" :key="series.key" :points="polyline(series.key)" fill="none"
        :stroke="series.color" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
      <text v-for="item in xLabels" :key="`x-${item.index}`" :x="x(item.index)" :y="props.height - 6" text-anchor="middle"
        class="fill-current text-[10px] opacity-60">{{ item.label }}</text>
    </svg>
  </div>
</template>
