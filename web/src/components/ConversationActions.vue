<script setup lang="ts">
import type { Conversation } from '@/types'

// 会话列表项的行内操作：进行中只能归档；已归档才能恢复或删除（后端要求先归档再删除）。
defineProps<{ item: Conversation }>()
defineEmits<{
  archive: [item: Conversation]
  unarchive: [item: Conversation]
  remove: [item: Conversation]
}>()
</script>

<template>
  <button
    v-if="item.status === 'active'"
    type="button"
    class="rail-item-action"
    title="归档"
    aria-label="归档对话"
    @click.stop="$emit('archive', item)"
  >⇩</button>
  <template v-else>
    <button type="button" class="rail-item-action" title="恢复" aria-label="恢复对话" @click.stop="$emit('unarchive', item)">⇧</button>
    <button type="button" class="rail-item-action danger" title="彻底删除" aria-label="彻底删除对话" @click.stop="$emit('remove', item)">✕</button>
  </template>
</template>
