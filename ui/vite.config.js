import { fileURLToPath, URL } from 'node:url'

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // Straight into the Go embed directory, so `make ui && go build` is the
    // whole pipeline. Absolute base (the default) keeps asset URLs correct on
    // deep SPA routes like /s/<session>/<id>.
    outDir: '../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // changeOrigin rewrites Host to localhost:5600; the dashboard's guard
      // requires a loopback Host, and without this the proxied request would
      // still carry the dev server's own host and be rejected.
      '/api': { target: 'http://localhost:5600', changeOrigin: true },
    },
  },
})
