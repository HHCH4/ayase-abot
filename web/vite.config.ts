import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import { readFileSync } from 'node:fs'

const pkg = JSON.parse(readFileSync(fileURLToPath(new URL('./package.json', import.meta.url)), 'utf8')) as { version: string }

// 开发环境代理 API，生产环境由 Go 的 go:embed 直接提供同一份静态产物。
export default defineConfig({
  plugins: [vue()],
  base: '/',
  // 顶栏展示的版本号与 package.json 保持单一来源。
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
  },
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:6185',
    },
  },
  build: {
    outDir: fileURLToPath(new URL('../internal/webui/dist', import.meta.url)),
    emptyOutDir: true,
  },
})
