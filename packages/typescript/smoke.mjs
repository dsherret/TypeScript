// Smoke test: does the migrated in-process Wasm reactor actually run on TypeScript main?
import { createVirtualFileSystem } from "./dist/api/fs.js";
import { createWasmAPI } from "./dist/api/wasm/api.js";

const files = {
  "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true, target: "es2022", module: "esnext" }, include: ["**/*.ts"] }),
  "/a.ts": "export const n: number = [1,2,3].reduce((a,b)=>a+b,0);\nexport const bad: string = 123;\n",
};
const api = createWasmAPI({ cwd: "/", fs: createVirtualFileSystem(files) });
console.log("version:", api.version ?? "(lazy)");
const snap = api.updateSnapshot({ openProject: "/tsconfig.json" });
const project = snap.getProject("/tsconfig.json");
const diags = project.program.getSemanticDiagnostics();
console.log("diagnostics:", diags.length);
for (const d of diags.slice(0, 3)) {
  const m = typeof d.messageText === "string" ? d.messageText : d.messageText?.messageText;
  console.log("  -", d.code, m);
}
if (diags.length !== 1 || diags[0].code !== 2322) throw new Error("expected exactly one 2322 error");
console.log("SMOKE OK: in-process Wasm reactor parses + checks on TypeScript main");
