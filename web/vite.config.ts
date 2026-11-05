import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 版本号由构建时注入：release 传 tag（v2.0.2），本地/CI 默认 dev。
// 以前硬编码在 Sidebar 里，于是页脚永远停在 v1.1.0。
const rawVersion = process.env.APP_VERSION || 'dev'
const appVersion = rawVersion.replace(/^v/, '')

export default defineConfig({
  plugins: [react()],
  define: {
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  server: {
    proxy: {
      '/v1': 'http://localhost:8080',
      '/admin': 'http://localhost:8080',
      '/anthropic': 'http://localhost:8080',
    }
  },
  build: {
    outDir: '../static',
    emptyOutDir: true,
  }
})
