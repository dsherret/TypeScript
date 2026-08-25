import { createVirtualFileSystem } from "./dist/api/fs.js";
import { createWasmAPI } from "./dist/api/wasm/api.js";
const files = {
  "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true, target: "es2022", module: "esnext" }, include: ["**/*.ts"] }),
  "/a.ts": "export const foo = 1;\nexport function bar() {}\nexport class Baz {}\n",
  "/b.ts": "export const qux = 2;\n",
};
const api = createWasmAPI({ cwd: "/", fs: createVirtualFileSystem(files) });
const snap = api.updateSnapshot({ openProject: "/tsconfig.json" });
const project = snap.getProject("/tsconfig.json");
const result = project.checker.getExportedSymbolsOfFiles([{ fileName: "/a.ts" }, { fileName: "/b.ts" }]);
console.log("a.ts exports:", result[0].map(e => e.name).sort().join(","));
console.log("b.ts exports:", result[1].map(e => e.name).sort().join(","));
if (result[0].length !== 3 || result[1].length !== 1) throw new Error("unexpected export counts");
console.log("EXPORTS SMOKE OK: batched getExportedSymbolsOfFiles works through the reactor");
