// End-to-end smoke test: drive the real sync API through the in-process Wasm
// reactor over an in-memory file system. Run from _packages/native-preview:
//   npm run node -- test/wasm-smoke.ts
import { SyntaxKind } from "@typescript/native-preview/unstable/ast";
import { createVirtualFileSystem } from "@typescript/native-preview/unstable/fs";
import assert from "node:assert";
import { readFileSync } from "node:fs";
import { createWasmAPI } from "../src/api/wasm/api.ts";

// The package no longer reads the module itself: where it comes from is the
// host's decision, so a Node host reads the bytes and hands them over.
const wasm = readFileSync(new URL("../dist/typescript.wasm", import.meta.url));

const files = {
    "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true } }),
    "/src/index.ts": `export const x: number = 1;\nexport const y = x + 2;\nexport function add(a: number, b: number) { return a + b; }\n`,
};

const api = createWasmAPI({ wasm, cwd: "/", fs: createVirtualFileSystem(files) });

const snapshot = api.updateSnapshot({ openProject: "/tsconfig.json" });
const project = snapshot.getProject("/tsconfig.json");
assert.ok(project, "expected a project for /tsconfig.json");

const program = project.program;
const sourceFile = program.getSourceFile("/src/index.ts");
assert.ok(sourceFile, "expected source file /src/index.ts");
console.log("fileName:", sourceFile.fileName);
console.log("statements:", sourceFile.statements.length);

let identifiers = 0;
sourceFile.forEachChild(function visit(node) {
    if (node.kind === SyntaxKind.Identifier) identifiers++;
    node.forEachChild(visit);
});
console.log("identifiers:", identifiers);
assert.ok(identifiers > 0, "expected to walk identifiers");

// Type-check through the checker (exercises the checker + FS callbacks for libs).
const checker = project.checker;
const xSymbol = checker.getSymbolAtPosition("/src/index.ts", "export const ".length);
assert.ok(xSymbol, "expected symbol for x");
console.log("symbol:", xSymbol.name);
const xType = checker.getTypeOfSymbol(xSymbol);
assert.ok(xType, "expected type for x");
console.log("typeof x:", checker.typeToString(xType));

// Semantic diagnostics should be empty for this valid program.
const diagnostics = program.getSemanticDiagnostics("/src/index.ts");
console.log("semantic diagnostics:", diagnostics.length);

api.close();
console.log("WASM E2E OK");
