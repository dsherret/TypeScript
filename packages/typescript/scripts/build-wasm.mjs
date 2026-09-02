// Builds the in-process WebAssembly reactor (tsc/cmd/tsgo-wasm) to
// dist/typescript.wasm, where the loader in src/api/wasm/api.ts reads it from.
//
//   node packages/typescript/scripts/build-wasm.mjs
//
// The release flags match the native builds in Herebyfile.mjs: no symbol table
// or DWARF, and no build paths embedded.
import { execFileSync } from "node:child_process";
import {
    mkdirSync,
    statSync,
} from "node:fs";
import {
    dirname,
    join,
} from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = fileURLToPath(new URL("../", import.meta.url));
const repoRoot = join(packageRoot, "..", "..");
const out = join(packageRoot, "dist", "typescript.wasm");
mkdirSync(dirname(out), { recursive: true });

execFileSync("go", ["build", "-buildmode=c-shared", "-trimpath", "-ldflags=-s -w", "-o", out, "./tsc/cmd/tsgo-wasm"], {
    cwd: repoRoot,
    stdio: "inherit",
    env: { ...process.env, GOOS: "wasip1", GOARCH: "wasm", CGO_ENABLED: "0" },
});

console.log(`built ${out} (${(statSync(out).size / 1024 / 1024).toFixed(2)} MiB)`);
