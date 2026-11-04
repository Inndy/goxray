// vp migrate: merged .oxlintrc.json (1ac939bf66775ffc654ae3d8a4be4dd9).
import { fileURLToPath, URL } from 'node:url'

import { defineConfig, lazyPlugins } from 'vite-plus'
import vue from '@vitejs/plugin-vue'

// https://vite.dev/config/
export default defineConfig({
  fmt: {
    semi: false,
    singleQuote: true,
  },
  lint: {
    plugins: ['eslint', 'unicorn', 'oxc', 'vue'],
    categories: {
      correctness: 'error',
    },
    env: {
      browser: true,
      builtin: true,
    },
    // <script setup> compiler macros: eslint-plugin-vue used to declare these,
    // and the merged no-undef rule does not know about them.
    globals: {
      defineProps: 'readonly',
      defineEmits: 'readonly',
      defineExpose: 'readonly',
      defineModel: 'readonly',
      defineOptions: 'readonly',
      defineSlots: 'readonly',
      withDefaults: 'readonly',
    },
    ignorePatterns: ['**/dist/**', '**/dist-ssr/**', '**/coverage/**'],
    rules: {
      'no-case-declarations': 'error',
      'no-empty': 'error',
      'no-fallthrough': 'error',
      'no-prototype-builtins': 'error',
      'no-redeclare': 'error',
      'no-regex-spaces': 'error',
      'no-undef': 'error',
      'no-unexpected-multiline': 'error',
      'no-useless-assignment': 'error',
      'preserve-caught-error': 'error',
      'vite-plus/prefer-vite-plus-imports': 'error',
    },
    options: {
      typeAware: true,
      typeCheck: true,
    },
    jsPlugins: [
      {
        name: 'vite-plus',
        specifier: 'vite-plus/oxlint-plugin',
      },
    ],
  },
  plugins: lazyPlugins(() => [vue()]),
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
