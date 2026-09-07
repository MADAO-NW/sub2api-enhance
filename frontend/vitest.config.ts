import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
export default defineConfig({plugins:[vue()],resolve:{alias:{'@':fileURLToPath(new URL('./src',import.meta.url)),'vue-i18n':'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js'}},test:{environment:'jsdom',include:['src/**/*.spec.ts'],setupFiles:['./src/test-setup.ts']}})
