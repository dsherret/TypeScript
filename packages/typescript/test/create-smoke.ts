// End-to-end smoke test for the auto-selecting entry point: it must prefer the
// native subprocess when a binary is present and fall back to the in-process
// Wasm reactor when one is not. Run from packages/typescript:
//   npm run node -- test/create-smoke.ts

import assert from "node:assert";
import { createVirtualFileSystem } from "../src/api/fs.ts";
import {
    type Backend,
    createAPIWithBackend,
    isNativeBackendAvailable,
    selectBackend,
} from "../src/api/create.ts";

function makeFiles() {
    return {
        "/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true } }),
        "/src/index.ts": `export const x: number = 1;\nexport const y = x + 2;\n`,
        "/src/bad.ts": `export const bad: string = 123;\n`,
    };
}

// Drives a session far enough to prove the backend actually compiles and checks.
function exercise(backend: Backend): void {
    const { api, backend: chosen } = createAPIWithBackend({
        backend,
        cwd: "/",
        fs: createVirtualFileSystem(makeFiles()),
    });
    assert.equal(chosen, backend, `expected the ${backend} backend`);

    const snapshot = api.updateSnapshot({ openProject: "/tsconfig.json" });
    const project = snapshot.getProject("/tsconfig.json");
    assert.ok(project, "expected a project for /tsconfig.json");

    const clean = project.program.getSemanticDiagnostics("/src/index.ts");
    assert.equal(clean.length, 0, "a valid file should have no semantic diagnostics");

    const bad = project.program.getSemanticDiagnostics("/src/bad.ts");
    assert.equal(bad.length, 1, "the bad file should have exactly one diagnostic");
    assert.equal(bad[0].code, 2322, "and it should be the 2322 type error");

    api.close();
    console.log(`  ${backend}: checked both files, TS2322 caught`);
}

const nativeAvailable = isNativeBackendAvailable();
console.log("native available here:", nativeAvailable);
console.log("auto-selected backend:", selectBackend());

// An explicit backend always wins, regardless of what is installed.
assert.equal(selectBackend({ backend: "wasm" }), "wasm", "an explicit backend must win");

// When the native binary is present (built at built/local/tsc(.exe) in this
// repo), auto selection must prefer it; otherwise it must fall back to wasm.
assert.equal(selectBackend(), nativeAvailable ? "native" : "wasm", "auto selection must prefer native only when available");

if (nativeAvailable) {
    console.log("native backend:");
    exercise("native");
}
else {
    console.log("native binary not built — skipping native, verifying wasm fallback");
}
console.log("wasm backend:");
exercise("wasm");

console.log("CREATE E2E OK: native preferred when present, wasm always works");
