#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const runtimeRoot = join(repoRoot, "runtime");
const frontendRoot = join(repoRoot, "frontend", "src");
const modulePrefix = "github.com/somewhere-tech/sessions/runtime/";
const allowListPath = join(repoRoot, "scripts", "import-boundaries.txt");
const require = createRequire(import.meta.url);
const ts = require(join(repoRoot, "frontend", "node_modules", "typescript"));
const printCurrent = process.argv.slice(2).includes("--print-current");

const currentEdges = goProductEdges();
if (printCurrent) {
  process.stdout.write([...currentEdges].sort().join("\n") + "\n");
  process.exit(0);
}

const allowedEdges = readAllowList();
const unexpected = [...currentEdges].filter((edge) => !allowedEdges.has(edge)).sort();
const stale = [...allowedEdges].filter((edge) => !currentEdges.has(edge)).sort();
const frontendViolations = forbiddenFrontendImports();

if (unexpected.length || stale.length || frontendViolations.length) {
  if (unexpected.length) {
    process.stderr.write("Go product imports missing from scripts/import-boundaries.txt:\n");
    for (const edge of unexpected) process.stderr.write(`  ${edge}\n`);
  }
  if (stale.length) {
    process.stderr.write("Stale Go product imports in scripts/import-boundaries.txt:\n");
    for (const edge of stale) process.stderr.write(`  ${edge}\n`);
  }
  if (frontendViolations.length) {
    process.stderr.write("Frontend lib/api modules may not import components/hooks:\n");
    for (const violation of frontendViolations) process.stderr.write(`  ${violation}\n`);
  }
  process.exit(1);
}

process.stdout.write(
  `Import boundaries passed: ${currentEdges.size} declared Go product edges; frontend lib/api point inward.\n`,
);

function goProductEdges() {
  const edges = new Set();
  for (const goos of ["darwin", "linux", "windows"]) {
    const listed = spawnSync(
      "go",
      [
        "list",
        "-deps",
        "-f",
        "{{.ImportPath}}\t{{join .Imports \",\"}}",
        "./cmd/...",
        "./internal/...",
      ],
      {
        cwd: runtimeRoot,
        encoding: "utf8",
        env: { ...process.env, CGO_ENABLED: "0", GOARCH: "amd64", GOFLAGS: "-buildvcs=false", GOOS: goos },
      },
    );
    if (listed.error) throw listed.error;
    if (listed.status !== 0) throw new Error(listed.stderr.trim() || `go list failed for ${goos}`);
    for (const line of listed.stdout.split(/\r?\n/)) {
      if (!line) continue;
      const [importer, imports = ""] = line.split("\t");
      if (!importer.startsWith(modulePrefix)) continue;
      for (const imported of imports.split(",")) {
        if (!imported.startsWith(modulePrefix)) continue;
        edges.add(`${importer.slice(modulePrefix.length)} ${imported.slice(modulePrefix.length)}`);
      }
    }
  }
  return edges;
}

function readAllowList() {
  const edges = new Set();
  for (const [index, sourceLine] of readFileSync(allowListPath, "utf8").split(/\r?\n/).entries()) {
    const line = sourceLine.trim();
    if (line === "" || line.startsWith("#")) continue;
    if (!/^(?:cmd|internal)\/\S+ internal\/\S+$/.test(line)) {
      throw new Error(`${allowListPath}:${index + 1}: expected '<importer> <imported-internal-package>'`);
    }
    if (edges.has(line)) throw new Error(`${allowListPath}:${index + 1}: duplicate boundary`);
    edges.add(line);
  }
  return edges;
}

function forbiddenFrontendImports() {
  const listed = spawnSync(
    "git",
    ["ls-files", "--cached", "--others", "--exclude-standard", "--", "frontend/src/lib", "frontend/src/api"],
    { cwd: repoRoot, encoding: "utf8" },
  );
  if (listed.error) throw listed.error;
  if (listed.status !== 0) throw new Error(listed.stderr.trim() || "git ls-files failed");

  const violations = [];
  for (const path of listed.stdout.split(/\r?\n/).filter((item) => /\.tsx?$/.test(item))) {
    const source = ts.createSourceFile(
      path,
      readFileSync(join(repoRoot, path), "utf8"),
      ts.ScriptTarget.Latest,
      true,
      path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
    );
    visitImports(source, source, path, violations);
  }
  return violations.sort();
}

function visitImports(node, source, path, violations) {
  const specifier = moduleSpecifier(node);
  if (specifier?.startsWith(".")) {
    const target = relative(frontendRoot, resolve(repoRoot, dirname(path), specifier)).replaceAll("\\", "/");
    if (/^(?:components|hooks)(?:\/|$)/.test(target)) {
      const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
      violations.push(`${path}:${line} imports ${specifier}`);
    }
  }
  ts.forEachChild(node, (child) => visitImports(child, source, path, violations));
}

function moduleSpecifier(node) {
  if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier && ts.isStringLiteral(node.moduleSpecifier)) {
    return node.moduleSpecifier.text;
  }
  if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument) && ts.isStringLiteral(node.argument.literal)) {
    return node.argument.literal.text;
  }
  if (ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword && node.arguments.length === 1 && ts.isStringLiteral(node.arguments[0])) {
    return node.arguments[0].text;
  }
  return null;
}
