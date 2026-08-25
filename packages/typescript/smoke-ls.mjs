// Verify the restored LS edit features (rename, format) work through the migrated reactor.
import { createVirtualFileSystem } from "./dist/api/fs.js";
import { createWasmAPI } from "./dist/api/wasm/api.js";
const files = {
  "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true, target: "es2022" }, include: ["**/*.ts"] }),
  "/a.ts": "export class Greeter {\n  greet(name: string) { return `hi ${name}`; }\n}\nconst g = new Greeter();\ng.greet('x');\n",
};
const api = createWasmAPI({ cwd: "/", fs: createVirtualFileSystem(files) });
const snap = api.updateSnapshot({ openProject: "/tsconfig.json" });
const project = snap.getProject("/tsconfig.json");

// rename Greeter -> Salutation (position of "Greeter" in the class decl, offset 13)
const renameEdits = project.rename("/a.ts", 13, "Salutation");
const totalEdits = renameEdits.reduce((n, f) => n + f.edits.length, 0);
console.log("rename edits across files:", renameEdits.length, "edits:", totalEdits);
if (totalEdits < 2) throw new Error("expected rename to touch the declaration + the `new Greeter()` reference");

// format the document
const fmtEdits = project.formatDocument("/a.ts");
console.log("format edits:", fmtEdits.length);

console.log("LS SMOKE OK: rename + format run through the migrated reactor");
