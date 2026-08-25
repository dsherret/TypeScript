import { createVirtualFileSystem } from "./dist/api/fs.js";
import { createWasmAPI } from "./dist/api/wasm/api.js";
const api = createWasmAPI({ cwd: "/", fs: createVirtualFileSystem({}) });
// parse-without-snapshot: no project needed, just text
const sf = api.parseSourceFile({ fileName: "/x.ts" }, "export const a = 1;\nexport function f() {}\n");
console.log("kind:", sf.kind, "statements:", sf.statements.length);
if (sf.statements.length !== 2) throw new Error("expected 2 statements");
console.log("PARSE SMOKE OK: parseSourceFile (no snapshot) works through the reactor");
