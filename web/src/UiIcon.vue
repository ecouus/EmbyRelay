<script setup lang="ts">
defineProps<{ name: string }>()
const paths: Record<string, string> = {
 overview: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
 'user-sources': 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8 M17 8h5 M19.5 5.5v5',
 upstreams: 'M4 3h16v7H4z M4 14h16v7H4z M7 6.5h.01 M7 17.5h.01 M11 6.5h6 M11 17.5h6',
 routes: 'M4 7h16l-4-4 M20 17H4l4 4 M20 7l-4 4 M4 17l4-4',
 relays: 'M9 3h6v6H9z M3 16h6v5H3z M15 16h6v5h-6z M12 9v4 M6 16v-3h12v3',
 subscriptions: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M9 3a4 4 0 1 0 0 8 4 4 0 0 0 0-8 M17 4a4 4 0 0 1 0 7 M22 21v-2a4 4 0 0 0-3-3.87',
 sessions: 'M9 7l8 5-8 5z M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20',
 traffic: 'M4 3v18h17 M8 16v-5 M13 16V7 M18 16V4',
 diagnostics: 'M12 3L2 21h20z M12 9v5 M12 17h.01',
 audit: 'M5 3h14v18H5z M8 7h8 M8 12h8 M8 17h5',
 settings: 'M12 3l2 3 4 .5.5 4 2.5 2.5-2.5 2.5-.5 4-4 .5-2 3-2-3-4-.5-.5-4L3 13l2.5-2.5.5-4 4-.5z M12 9a4 4 0 1 0 0 8 4 4 0 0 0 0-8',
 devices: 'M4 3h16v12H4z M8 21h8 M12 15v6 M7 7h10 M7 11h6',
 menu: 'M4 6h16 M4 12h16 M4 18h16', close: 'M6 6l12 12 M18 6L6 18',
 arrow: 'M5 12h14 M14 7l5 5-5 5', shield: 'M12 2l8 4v6c0 5-8 10-8 10S4 17 4 12V6z M8 12l3 3 5-6',
}
</script>
<template><svg class="ui-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path :d="paths[name] || paths.overview" /></svg></template>
<style scoped>.ui-icon{width:20px;height:20px;flex-shrink:0;vertical-align:middle}</style>
