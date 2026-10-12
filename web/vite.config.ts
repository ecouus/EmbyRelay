import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
export default defineConfig({ plugins: [vue()], build: { rollupOptions: { output: { manualChunks: { 'naive-ui': ['naive-ui'], 'vue': ['vue'] } } } }, server: { proxy: { '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false } } } })
