import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
export default defineConfig({ plugins: [react()], test: { server: { deps: { inline: ['@uppy/tus', 'tus-js-client'] } }, environment: 'jsdom', setupFiles: ['./src/test/setup.ts'], restoreMocks: true } });
