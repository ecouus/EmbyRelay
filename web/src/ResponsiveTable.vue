<script setup lang="ts">
import { computed, h, ref } from 'vue'
import { NDataTable, NPagination, type DataTableColumns } from 'naive-ui'
const props = defineProps<{ columns: DataTableColumns<any>; data: any[]; pagination?: { pageSize: number }; bordered?: boolean }>()
const currentPage = ref(1)
const pageCount = computed(() => Math.max(1, Math.ceil(props.data.length / (props.pagination?.pageSize || props.data.length || 1))))
const activePage = computed(() => Math.min(currentPage.value, pageCount.value))
const rows = computed(() => props.pagination ? props.data.slice((activePage.value - 1) * props.pagination.pageSize, activePage.value * props.pagination.pageSize) : props.data)
const fields = computed(() => props.columns.filter(column => 'key' in column))
const Cell = (p: { column: any; row: any; index: number }) => p.column.render ? p.column.render(p.row, p.index) : h('span', String(p.row[p.column.key] ?? '—'))
</script>

<template>
  <div class="responsive-table">
    <n-data-table class="responsive-table-desktop" :columns="columns" :data="data" :bordered="bordered ?? false" :pagination="pagination" table-layout="fixed" />
    <div class="responsive-table-mobile">
      <article v-for="(row,index) in rows" :key="row.id ?? index" class="record-card">
        <dl>
          <div v-for="column in fields" :key="String(column.key)" :class="{'record-actions':column.key==='actions'}">
            <dt>{{ column.title }}</dt>
            <dd><Cell :column="column" :row="row" :index="(activePage - 1) * (pagination?.pageSize || 0) + index" /></dd>
          </div>
        </dl>
      </article>
      <div v-if="!data.length" class="record-empty">暂无数据</div>
      <n-pagination v-if="pagination && pageCount > 1" :page="activePage" :page-count="pageCount" :page-slot="3" size="small" @update:page="currentPage=$event" />
    </div>
  </div>
</template>
