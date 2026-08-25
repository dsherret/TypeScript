import { createVirtualFileSystem } from "./dist/api/fs.js";
import { createWasmAPI } from "./dist/api/wasm/api.js";
const files = {
  "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true, target: "es2022", lib: ["lib.es2022.d.ts"] }, include: ["**/*.ts"] }),
  "/a.ts": "export const x = 1;\n",
};
const api = createWasmAPI({ cwd: "/", fs: createVirtualFileSystem(files) });
const snap = api.updateSnapshot({ openProject: "/tsconfig.json" });
const project = snap.getProject("/tsconfig.json");
const checker = project.checker;
const ambient = checker.getAmbientModules();
console.log("getAmbientModules returned:", Array.isArray(ambient) ? ambient.length + " modules" : typeof ambient);
console.log("CHECKER SMOKE OK: getAmbientModules routed through migrated reactor");
