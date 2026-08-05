// Lint configuration for the shared Sessions frontend.
//
// The rule this file exists for is `react-hooks/exhaustive-deps`. Sessions'
// live surfaces — the mux terminal stream, the resume dialog, machine identity
// refresh — are all effects keyed on a connection or a selection. When such an
// effect's dependency list silently drifts from what its body reads, the UI
// does not throw; it keeps rendering a stream that is no longer connected to
// anything. That failure is invisible in typecheck, build, and the smoke
// suite, so it is enforced here as an error rather than a warning.
//
// `// eslint-disable-next-line react-hooks/exhaustive-deps` suppressions
// predate this config. Each one now has to be justified against a rule that
// actually runs; an unnecessary suppression is itself reported.

import js from '@eslint/js';
import globals from 'globals';
import tseslint from 'typescript-eslint';
import reactHooks from 'eslint-plugin-react-hooks';

export default tseslint.config(
  {
    // Build output, dependencies, and the plain-Node build/smoke scripts.
    // The scripts are checked by their own smoke runs, not by the React rules.
    // `vite.config.js` is emitted by `tsc -b` from vite.config.ts and is
    // gitignored — lint the source below, never the generated copy.
    ignores: ['dist/**', 'node_modules/**', 'scripts/**', 'public/**', 'vite.config.js']
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    // The Vite config runs in Node, not the browser.
    files: ['vite.config.ts'],
    languageOptions: { globals: globals.node }
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'module',
      globals: { ...globals.browser, ...globals.es2022 },
      parserOptions: {
        ecmaFeatures: { jsx: true }
      }
    },
    plugins: { 'react-hooks': reactHooks },
    linterOptions: {
      // A suppression that no longer suppresses anything is stale review
      // signal. Report it so it gets removed with the code it guarded.
      reportUnusedDisableDirectives: 'error'
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // The point of this config.
      'react-hooks/exhaustive-deps': 'error',
      'react-hooks/rules-of-hooks': 'error',

      // TypeScript already enforces these with better type information, and
      // its versions understand type-only positions and declaration merging.
      'no-unused-vars': 'off',
      'no-undef': 'off',
      // Unused values are reported, but an underscore prefix is the codebase's
      // existing, deliberate "intentionally discarded" marker (see the token
      // stripping in lib/servers.ts).
      '@typescript-eslint/no-unused-vars': ['error', {
        argsIgnorePattern: '^_',
        varsIgnorePattern: '^_',
        caughtErrors: 'none',
        destructuredArrayIgnorePattern: '^_'
      }],
      // `any` is a type-quality question, not a correctness gate. Left as a
      // warning so it does not block the error-level rules above.
      '@typescript-eslint/no-explicit-any': 'warn',

      // Sessions renders terminal output. Matching ESC, BEL, and the C0 range
      // is the job of ansi.ts, asciiTable.ts, copyText.ts, contentRender.ts and
      // detectMultiChoice.ts — a control character in those patterns is the
      // intent, not a typo, and the default rule flags every one of them.
      'no-control-regex': 'off'
    }
  },

  // ───────────────────────────────────────────────────────────────────────
  // Known pre-existing violations, listed file by file so the gate above is
  // usable on new work today. These are NOT accepted as correct: each one is
  // an open cleanup, deliberately left untouched rather than mass-edited
  // blind. Delete an entry here as soon as its file is fixed — the config is
  // meant to shrink.
  //
  //   FleetView.tsx:349        useEffect drops `server`
  //   NewSessionDialog.tsx:273 useEffect drops `parentSession`
  //   RemoteView.tsx:261       useEffect drops `latestFailedSend`
  //   SearchView.tsx:210       useEffect drops `query`
  //   detectMultiChoice.ts:48,79 redundant escapes in picker regexes
  //   filePaths.ts:11          redundant escape in the path regex
  //
  // Each is scoped to the single rule it trips, so every other rule — and any
  // NEW violation of a different rule in these same files — still fails.
  {
    files: [
      'src/components/FleetView.tsx',
      'src/components/NewSessionDialog.tsx',
      'src/components/RemoteView.tsx',
      'src/components/SearchView.tsx'
    ],
    rules: { 'react-hooks/exhaustive-deps': 'off' }
  },
  {
    files: ['src/lib/detectMultiChoice.ts', 'src/lib/filePaths.ts'],
    rules: { 'no-useless-escape': 'off' }
  }
);
