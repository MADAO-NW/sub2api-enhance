import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
export default defineConfig({ base: '/enhance/', plugins: [vue()], resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } }, build: { outDir: '../backend/internal/web/dist', emptyOutDir: true }, server: { proxy: { '/enhance/api': { target: process.env.VITE_ENHANCE_PROXY_TARGET || 'http://127.0.0.1:18081', changeOrigin: false } } } })
