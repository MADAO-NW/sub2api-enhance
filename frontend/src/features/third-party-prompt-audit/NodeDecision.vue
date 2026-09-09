<script setup lang="ts">
import { computed } from 'vue'
import type { DecisionConfig, ModelResult } from '@/api/admin/third-party-prompt-audit'
import { useAuditLabels } from './labels'

const props = defineProps<{ model: ModelResult; config?: DecisionConfig }>()
const label = useAuditLabels()
// 尚未提交的检查点保留原始片段，列表与正式结果直接使用后端派生字段。
const maxSegment = computed(() => props.model.max_segment_confidence !== undefined
  ? props.model.max_segment_confidence
  : props.model.segments?.reduce<number | null>((max, item) => max === null ? item.result.confidence : Math.max(max, item.result.confidence), null))
</script>

<template>
  <div class="space-y-1 text-sm" data-test="node-decision">
    <p v-if="model.skipped" class="text-gray-500">{{ label(model.skip_reason || 'aggregation_decided') }}</p>
    <p v-else-if="model.error" class="text-red-600 dark:text-red-400">{{ label('unclassified') }} · {{ model.error.message }} · {{ model.error.code }}</p>
    <template v-else-if="model.basis === 'segments_all_pass'">
      <p>{{ label('segments_all_pass') }}</p>
      <p>{{ label('maxSegmentScore') }}: {{ maxSegment ?? label('noValidScore') }} · {{ label('triggerThreshold') }}: {{ config?.review_threshold ?? '—' }}</p>
      <p>{{ label('jointNotRun') }}</p>
    </template>
    <template v-else-if="model.confidence != null">
      <p>{{ label(model.reused ? 'reusedJoint' : 'jointScore') }}: {{ model.confidence }} · {{ model.decision ? label(model.decision) : '—' }}</p>
      <p>{{ label('rawThresholds') }}: {{ config?.review_threshold ?? '—' }} / {{ config?.block_threshold ?? '—' }}</p>
      <p v-if="model.joint_attempt_id && !model.reused">{{ label('sourceReference') }} · Attempt #{{ model.joint_attempt_id }}</p>
    </template>
    <p v-else>{{ label('noValidScore') }}</p>
    <p v-if="config">{{ label('decisionRevision') }}: {{ config.revision }}</p>
  </div>
</template>
