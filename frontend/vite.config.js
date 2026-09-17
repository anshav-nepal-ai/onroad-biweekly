import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: `http://localhost:${process.env.PORT || 8082}`,
        changeOrigin: true,
        onError(err, req, res) {
          res.writeHead(503, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify({
            status: 'building',
            message: 'Backend is starting, please wait...',
          }))
        },
      }
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  }
})
