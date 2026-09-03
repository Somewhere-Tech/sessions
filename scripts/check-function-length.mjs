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
for (const fn of functions) {
	const exceptionLimit = exceptions.get(`${fn.path}\t${fn.name}`);
	const excepted = exceptionLimit !== undefined;
	if (!options.report && (fn.length <= options.maxTypescript || excepted && fn.length <= exceptionLimit)) continue;
	process.stdout.write(`${fn.path}:${fn.line}:${fn.name}:${fn.length}\n`);
	if (fn.length > options.maxTypescript && (!excepted || fn.length > exceptionLimit)) violations += 1;
}

if (!options.report && (go.status !== 0 || violations > 0)) {
  process.exitCode = 1;
}

function parseOptions(args) {
  const options = { maxGo: 80, maxTypescript: 80, report: false };
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
		if (fields[0] === language) exceptions.set(`${fields[1]}\t${fields[2]}`, currentLines);
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
    visit(source, source, path, functions);
  }
  return functions;
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
    functions.push({ path, line: start, name: functionName(node, source, start), length: end - start + 1 });
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

function functionName(node, source, line) {
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
	return anonymousName(node, source, line);
}

function anonymousName(node, source, line) {
	let child = node;
	let parent = node.parent;
	while (parent && (ts.isParenthesizedExpression(parent) || ts.isAsExpression(parent))) {
		child = parent;
		parent = parent.parent;
	}

	let localName = `<anonymous@${line}>`;
	if (parent && ts.isCallExpression(parent)) {
		localName = parent.expression === child
			? "<iife>"
			: parent.expression.getText(source).replace(/\s+/g, "");
	}

	for (let ancestor = node.parent; ancestor; ancestor = ancestor.parent) {
		if (isFunctionWithBody(ancestor)) {
			const ancestorLine = source.getLineAndCharacterOfPosition(ancestor.getStart(source)).line + 1;
			return `${functionName(ancestor, source, ancestorLine)}/${localName}`;
		}
	}
	return localName;
}

function className(node, source) {
  return node?.name?.getText(source) || "<anonymous-class>";
}
