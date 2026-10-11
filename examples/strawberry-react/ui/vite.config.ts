import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 3100,
    proxy: {
      "/graphql": {
        target: "http://localhost:8100",
      },
    }
  }
})
