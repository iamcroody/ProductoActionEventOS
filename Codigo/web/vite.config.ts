import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

export default defineConfig({
  plugins: [preact()],
  // Absolute base so hashed asset URLs resolve from the site root on EVERY
  // route. With a relative base ('./'), a deep link or refresh on e.g.
  // /admin/menu makes the browser request /admin/assets/*.js, which the Go SPA
  // fallback answers with index.html (wrong MIME) → blank page. '/' fixes it.
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2020',
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
});
