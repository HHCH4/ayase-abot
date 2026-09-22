<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  name: string
  size?: number | string
  strokeWidth?: number
}>(), {
  size: 18,
  strokeWidth: 1.8,
})

const paths: Record<string, string[]> = {
  dashboard: ['M4 19V10', 'M10 19V4', 'M16 19v-7', 'M22 19H2'],
  bot: ['M8 8h8a3 3 0 0 1 3 3v5a3 3 0 0 1-3 3H8a3 3 0 0 1-3-3v-5a3 3 0 0 1 3-3Z', 'M12 4v4', 'M9 12h.01', 'M15 12h.01', 'M9 16h6', 'M2 13h3', 'M19 13h3'],
  terminal: ['M4 5h16v14H4z', 'm7 9 3 3-3 3', 'M13 15h4'],
  sparkles: ['m12 3 1.6 5.4L19 10l-5.4 1.6L12 17l-1.6-5.4L5 10l5.4-1.6L12 3Z', 'm19 16 .7 2.3L22 19l-2.3.7L19 22l-.7-2.3L16 19l2.3-.7L19 16Z'],
  settings: ['M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4Z', 'm19.4 15 .1.1a1.8 1.8 0 0 1-2.5 2.5l-.1-.1a1.8 1.8 0 0 0-3 .9v.2a1.8 1.8 0 0 1-3.6 0v-.2a1.8 1.8 0 0 0-3-.9l-.1.1a1.8 1.8 0 0 1-2.5-2.5l.1-.1a1.8 1.8 0 0 0-.9-3h-.2a1.8 1.8 0 0 1 0-3.6h.2a1.8 1.8 0 0 0 .9-3l-.1-.1a1.8 1.8 0 0 1 2.5-2.5l.1.1a1.8 1.8 0 0 0 3-.9v-.2a1.8 1.8 0 0 1 3.6 0v.2a1.8 1.8 0 0 0 3 .9l.1-.1a1.8 1.8 0 0 1 2.5 2.5l-.1.1a1.8 1.8 0 0 0 .9 3h.2a1.8 1.8 0 0 1 0 3.6h-.2a1.8 1.8 0 0 0-.9 3Z'],
  book: ['M5 5.5A2.5 2.5 0 0 1 7.5 3H20v16H7.5A2.5 2.5 0 0 0 5 21.5v-16Z', 'M5 5.5v16', 'M8 7h8', 'M8 11h6'],
  monitor: ['M4 5h16v11H4z', 'M8 20h8', 'M12 16v4'],
  users: ['M16 20v-1.5a3.5 3.5 0 0 0-3.5-3.5h-1A3.5 3.5 0 0 0 8 18.5V20', 'M12 11a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z', 'M19 20v-1.2a3.2 3.2 0 0 0-2.4-3.1', 'M17 5.2a3 3 0 0 1 0 5.6'],
  chart: ['M4 19V5', 'M4 19h17', 'm7 15 3-4 3 2 5-7'],
  rules: ['M6 4h12', 'M6 9h12', 'M6 14h7', 'M6 19h5', 'm16 14 2 2 4-5'],
  calendar: ['M5 4h14a2 2 0 0 1 2 2v13H3V6a2 2 0 0 1 2-2Z', 'M8 2v4', 'M16 2v4', 'M3 9h18'],
  chat: ['M4 5h16v11H9l-5 4V5Z', 'M8 10h.01', 'M12 10h.01', 'M16 10h.01'],
  menu: ['M4 6h16', 'M4 12h16', 'M4 18h16'],
  plus: ['M12 5v14', 'M5 12h14'],
  folder: ['M3 6h7l2 2h9v11H3V6Z'],
  file: ['M6 3h8l4 4v14H6V3Z', 'M14 3v5h5'],
  paperclip: ['m8 12.5 5.7-5.7a3 3 0 0 1 4.2 4.2l-7.8 7.8a5 5 0 0 1-7.1-7.1l7.1-7.1'],
  close: ['m6 6 12 12', 'm18 6-12 12'],
  send: ['M4 12 20 4l-5 16-3-6-8-2Z', 'm12 14 8-10'],
  refresh: ['M20 11a8 8 0 1 0 1 5', 'M20 5v6h-6'],
  check: ['m5 12 4 4L19 6'],
  shield: ['m12 3 8 3v5c0 5-3.4 8.3-8 10-4.6-1.7-8-5-8-10V6l8-3Z', 'm9 12 2 2 4-4'],
  warning: ['M12 4 21 20H3L12 4Z', 'M12 9v5', 'M12 17h.01'],
  edit: ['m4 16-.8 4.8L8 20l11-11a2.8 2.8 0 0 0-4-4L4 16Z', 'm13.5 6.5 4 4'],
  trash: ['M4 7h16', 'M10 11v6', 'M14 11v6', 'm6 7 1 14h10l1-14', 'M9 7V4h6v3'],
  archive: ['M4 7h16', 'M5 7l1 13h12l1-13', 'M8 11h8', 'M9 4h6l1 3H8l1-3Z'],
  restore: ['M4 12a8 8 0 1 0 2.3-5.7', 'M4 5v7h7'],
  search: ['m21 21-4.4-4.4', 'M10.8 18a7.2 7.2 0 1 0 0-14.4 7.2 7.2 0 0 0 0 14.4Z'],
  'arrow-up': ['M12 19V5', 'm6 11 6-6 6 6'],
  'chevron-down': ['m6 9 6 6 6-6'],
  'chevron-up': ['m6 15 6-6 6 6'],
}

const iconPaths = computed(() => paths[props.name] || paths.sparkles)
</script>

<template>
  <svg
    class="app-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    :stroke-width="strokeWidth"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path v-for="(path, index) in iconPaths" :key="index" :d="path" />
  </svg>
</template>
