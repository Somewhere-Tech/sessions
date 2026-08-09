import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The capability suite is a separate config from vite.config.ts on purpose.
// vite.config.ts owns the dev server and the service-worker build hash; it
// throws at import time on some host configurations and it is not the place to
// describe a test environment.
//
// Environment: jsdom, not happy-dom.
//
// happy-dom is roughly twice as fast, and for a suite of pure-function tests it
// would be the right pick. This suite is the opposite of that: it mounts real
// product components and drives them the way a person does. The components
// reach for the parts of the DOM that a partial implementation gets wrong —
// textarea selectionStart/setSelectionRange (InputBar's upload-and-insert),
// Range/Selection, elementFromPoint, ResizeObserver, drag-and-drop
// DataTransfer, and the exact Response/Request/Headers semantics that
// src/api/sessionsd.ts reads. jsdom is the reference implementation that
// @testing-library targets. The point of this harness is that a red test means
// the product is broken; every environment gap is a false red that costs more
// than the seconds it saves.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: false,
    include: ['tests/capability/**/*.test.tsx'],
    setupFiles: ['./tests/capability/setup.ts'],
    // A capability test that hangs is a capability that hangs. Bound it.
    testTimeout: 15_000,
    hookTimeout: 15_000,
    // Fail loudly rather than silently passing an empty run.
    passWithNoTests: false,
    restoreMocks: true,
    css: false
  }
});
