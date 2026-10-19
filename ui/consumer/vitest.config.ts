import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Kept apart from vite.config.ts so tests don't load the Module Federation
// plugin: components render against this package's own React and SDK copies.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
