/// <reference types="vite/client" />

// Side-effect CSS imports — including dynamic `import('foo.css')` used
// for lazy-loading xterm's stylesheet into a code-split chunk. Vite
// handles these at build time; TS needs a module declaration.
declare module '*.css';
