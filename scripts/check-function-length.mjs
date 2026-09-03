#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(import.meta.url);
const ts = require(join(repoRoot, "frontend", "node_modules", "typescript"));
const options = parseOptions(process.argv.slice(2));
const exceptionsPath = join(repoRoot, "scripts", "function-length-exceptions.txt");
const exceptions = readExceptions(exceptionsPath, "typescript");

const goArgs = [
  "run",
  "./scripts/check-function-length.go",
  "--root",
  ".",
  "--max",
  String(options.maxGo),
  "--exceptions",
  "../scripts/function-length-exceptions.txt",
];
if (options.report) goArgs.push("--report");
const go = spawnSync("go", goArgs, {
  cwd: join(repoRoot, "runtime"),
  encoding: "utf8",
  env: { ...process.env, GOFLAGS: "-buildvcs=false" },
});
if (go.error) throw go.error;
process.stdout.write(go.stdout);
process.stderr.write(go.stderr);

const functions = typescriptFunctions();
functions.sort((left, right) =>
  right.length - left.length || left.path.localeCompare(right.path) || left.line - right.line,
);

let violations = 0;
const seenExceptions = new Set();
for (const fn of functions) {
  const key = `${fn.path}\t${fn.name}`;
  const exceptionLimit = exceptions.get(key);
  const excepted = exceptionLimit !== undefined;
  if (excepted) seenExceptions.add(key);
  if (!options.report && excepted && fn.length <= options.maxTypescript) {
    process.stderr.write(`unneeded TypeScript function exception: ${fn.path}:${fn.name}\n`);
    violations += 1;
    continue;
  }
  if (!options.report && (fn.length <= options.maxTypescript || excepted && fn.length <= exceptionLimit)) continue;
  process.stdout.write(`${fn.path}:${fn.line}:${fn.name}:${fn.length}\n`);
  if (fn.length > options.maxTypescript && (!excepted || fn.length > exceptionLimit)) violations += 1;
}

if (!options.report) {
  for (const key of exceptions.keys()) {
    if (!seenExceptions.has(key)) {
      process.stderr.write(`stale TypeScript function exception: ${key.replace("\t", ":")}\n`);
      violations += 1;
    }
  }
}

if (!options.report && (go.status !== 0 || violations > 0)) {
  process.exitCode = 1;
} else if (!options.report) {
  process.stdout.write(
    `Function lengths passed: Go <= ${options.maxGo}; TypeScript/TSX <= ${options.maxTypescript}; frozen exceptions did not grow.\n`,
  );
}

function parseOptions(args) {
  const options = { maxGo: 80, maxTypescript: 120, report: false };
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === "--report") {
      options.report = true;
    } else if (argument === "--max-go") {
      options.maxGo = positiveInteger(args[++index], argument);
    } else if (argument === "--max-typescript") {
      options.maxTypescript = positiveInteger(args[++index], argument);
    } else {
      throw new Error(`unknown argument: ${argument}`);
    }
  }
  return options;
}

function positiveInteger(value, option) {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0 || String(parsed) !== value) {
    throw new Error(`${option} requires a positive integer`);
  }
  return parsed;
}

function readExceptions(path, language) {
  const exceptions = new Map();
  if (!existsSync(path)) return exceptions;
  for (const [index, sourceLine] of readFileSync(path, "utf8").split(/\r?\n/).entries()) {
    const line = sourceLine.trim();
    if (line === "" || line.startsWith("#")) continue;
    const fields = line.split(/\s+/);
    if (fields.length !== 4) {
      throw new Error(`${path}:${index + 1}: expected '<language> <path> <function> <current-lines>'`);
    }
    const currentLines = Number.parseInt(fields[3], 10);
    if (!Number.isSafeInteger(currentLines) || currentLines <= 0 || String(currentLines) !== fields[3]) {
      throw new Error(`${path}:${index + 1}: invalid function length '${fields[3]}'`);
    }
    if (fields[0] === language) {
      const key = `${fields[1]}\t${fields[2]}`;
      if (exceptions.has(key)) throw new Error(`${path}:${index + 1}: duplicate function exception`);
      exceptions.set(key, currentLines);
    }
  }
  return exceptions;
}

function typescriptFunctions() {
  const listed = spawnSync(
    "git",
    ["ls-files", "--cached", "--others", "--exclude-standard", "--", "frontend/src"],
    { cwd: repoRoot, encoding: "utf8" },
  );
  if (listed.error) throw listed.error;
  if (listed.status !== 0) throw new Error(listed.stderr.trim() || "git ls-files failed");

  const functions = [];
  for (const path of listed.stdout.split(/\r?\n/).filter(isHandwrittenTypescript)) {
    const absolutePath = join(repoRoot, path);
    const source = ts.createSourceFile(
      path,
      readFileSync(absolutePath, "utf8"),
      ts.ScriptTarget.Latest,
      true,
      path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
    );
    const fileFunctions = [];
    visit(source, source, path, fileFunctions);
    disambiguateNames(fileFunctions);
    functions.push(...fileFunctions);
  }
  return functions;
}

function disambiguateNames(functions) {
  const counts = new Map();
  const seen = new Map();
  for (const fn of functions) counts.set(fn.name, (counts.get(fn.name) || 0) + 1);
  for (const fn of functions) {
    if (counts.get(fn.name) < 2) continue;
    const ordinal = (seen.get(fn.name) || 0) + 1;
    seen.set(fn.name, ordinal);
    fn.name = `${fn.name}#${ordinal}`;
  }
}

function isHandwrittenTypescript(path) {
  return /\.tsx?$/.test(path) &&
    !/\.(?:test|generated)\.tsx?$/.test(path) &&
    !path.split(sep).includes("testdata");
}

function visit(node, source, path, functions) {
  if (isFunctionWithBody(node)) {
    const start = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
    const end = source.getLineAndCharacterOfPosition(node.end - 1).line + 1;
    const name = functionName(node, source);
    if (name) functions.push({ path, line: start, name, length: end - start + 1 });
  }
  ts.forEachChild(node, (child) => visit(child, source, path, functions));
}

function isFunctionWithBody(node) {
  return Boolean(node.body) && (
    ts.isFunctionDeclaration(node) ||
    ts.isFunctionExpression(node) ||
    ts.isArrowFunction(node) ||
    ts.isMethodDeclaration(node) ||
    ts.isConstructorDeclaration(node) ||
    ts.isGetAccessorDeclaration(node) ||
    ts.isSetAccessorDeclaration(node)
  );
}

function functionName(node, source) {
  if (ts.isConstructorDeclaration(node)) return `${className(node.parent, source)}.constructor`;
  if (ts.isMethodDeclaration(node) || ts.isGetAccessorDeclaration(node) || ts.isSetAccessorDeclaration(node)) {
    return `${className(node.parent, source)}.${node.name.getText(source)}`;
  }
  if (node.name) return node.name.getText(source);

  const parent = node.parent;
  if (ts.isVariableDeclaration(parent) || ts.isPropertyDeclaration(parent) || ts.isPropertyAssignment(parent)) {
    return parent.name.getText(source);
  }
  if (ts.isBinaryExpression(parent) && parent.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
    return parent.left.getText(source).replace(/\s+/g, "");
  }

  let callName = null;
  let child = node;
  for (let ancestor = parent; ancestor; child = ancestor, ancestor = ancestor.parent) {
    if (isFunctionWithBody(ancestor)) return null;
    if (ts.isVariableDeclaration(ancestor) || ts.isPropertyDeclaration(ancestor) || ts.isPropertyAssignment(ancestor)) {
      return ancestor.name.getText(source);
    }
    if (ts.isExportAssignment(ancestor)) return "<default-export>";
    if (ts.isCallExpression(ancestor) && ancestor.expression !== child && callName === null) {
      callName = ancestor.expression.getText(source).replace(/\s+/g, "");
    }
  }
  return callName;
}

function className(node, source) {
  return node?.name?.getText(source) || "<anonymous-class>";
}
