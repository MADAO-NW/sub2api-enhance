import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

export default defineConfig({
  base: '/enhance/',
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  define: {
    // 使用不依赖 eval 的 JIT 消息编译器，使翻译插值兼容严格 CSP。
    __INTLIFY_JIT_COMPILATION__: true,
    __INTLIFY_DROP_MESSAGE_COMPILER__: false,
  },
  build: {
    outDir: '../backend/internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/enhance/api': {
        target: process.env.VITE_ENHANCE_PROXY_TARGET || 'http://127.0.0.1:18081',
        changeOrigin: false,
      },
    },
  },
})
