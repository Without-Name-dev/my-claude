import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: '/code/',
  resolve: {
    alias: {
      '@/src': path.resolve(__dirname, 'src'),
      '@/components': path.resolve(__dirname, 'components'),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 10000,
    rollupOptions: {
      input: {
        main: path.resolve(__dirname, 'index.html'),
      },
      output: {
        manualChunks(id) {
          // shiki/motion used to be split into their own chunks. Vite injects
          // its `__vitePreload` helper into whichever chunk it lands in, and
          // that put the 9 MB shiki chunk (oniguruma wasm included) into the
          // entry's *static* import graph — every page load paid for a syntax
          // highlighter most conversations never use. Leaving them to Rollup
          // keeps them inside the lazily-imported markdown chunk (~59 KB) and
          // the first paint drops to ~85 KB gzipped.
          if (
            id.includes('node_modules/react') ||
            id.includes('node_modules/react-dom')
          ) {
            return 'vendor'
          }
          if (
            id.includes('node_modules/ai/') ||
            id.includes('node_modules/@ai-sdk/')
          ) {
            return 'ai-sdk'
          }
          if (
            id.includes('node_modules/qrcode') ||
            id.includes('node_modules/jsqr')
          ) {
            return 'qr'
          }
        },
      },
    },
  },
  server: {
    proxy: {
      '/web': 'http://localhost:3000',
      '/v1': 'http://localhost:3000',
      '/v2': 'http://localhost:3000',
    },
  },
})
