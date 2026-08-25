/**
 * Entry point for the in-process WebAssembly API.
 *
 * Instantiates the tsgo-wasm reactor and returns a fully synchronous {@link API}
 * backed by it — no subprocess, no native addon. Instantiation is synchronous, so
 * construction blocks until the session is ready and every subsequent request is
 * a plain function call.
 *
 * Nothing here reaches for a host built-in: WASI is answered by the shim in
 * ./wasi.ts, and the module either arrives from the caller — already compiled or
 * as bytes — or is read from beside this file through whatever the host offers
 * without an import. A browser has none of those, which is why a host that
 * targets one compiles the module itself and calls {@link setDefaultWasmModule}.
 */

import type { FileSystem } from "../fs.ts";
import type {
    APIOptions,
    ModuleNameResolver,
} from "../options.ts";
import { API } from "../sync/api.ts";
import {
    WasmChannel,
    type WasmExports,
} from "../wasmChannel.ts";
import {
    createWasiImports,
    type WasiShimOptions,
} from "./wasi.ts";

/**
 * The WebAssembly types this module uses, declared locally so the package does
 * not need the DOM library. The shapes match the standard ones.
 */
declare namespace WebAssembly {
    interface Module {}
    interface Instance {
        readonly exports: unknown;
    }
}
declare const WebAssembly: {
    Module: new (bytes: Uint8Array | ArrayBuffer) => WebAssembly.Module;
    Instance: new (module: WebAssembly.Module, imports: Record<string, unknown>) => WebAssembly.Instance;
};

/** The reactor module: already compiled, or the bytes to compile it from. */
export type WasmSource = WebAssembly.Module | Uint8Array | ArrayBuffer;

export interface WasmApiOptions {
    /**
     * The reactor module. Defaults to `typescript.wasm` beside this module, or
     * to whatever {@link setDefaultWasmModule} was last given.
     */
    wasm?: WasmSource;
    /** Current working directory used for module resolution. Defaults to "/". */
    cwd?: string;
    /**
     * Directory the default lib files are read from, through {@link fs}. Defaults
     * to the lib files bundled in the module.
     */
    defaultLibraryPath?: string;
    /** Whether the file system distinguishes case. Defaults to true. */
    useCaseSensitiveFileNames?: boolean;
    /** Virtual filesystem callbacks. */
    fs?: FileSystem;
    /**
     * Resolves a module specifier in place of the compiler. See the option of
     * the same name in ../options.ts for what an answer means.
     */
    resolveModuleName?: ModuleNameResolver;
    /** When true, collect per-request timing information. */
    collectTiming?: boolean;
    /**
     * Diagnostic hooks for the WASI shim. Only a Go runtime failure produces
     * output, and a working reactor never reaches an unsupported import, so this
     * is for observing that rather than for changing behaviour.
     */
    wasi?: Pick<WasiShimOptions, "onStdout" | "onStderr" | "onUnsupported">;
}

/**
 * Creates a synchronous {@link API} backed by the in-process WebAssembly reactor.
 */
export function createWasmAPI(options: WasmApiOptions = {}): API {
    const module = options.wasm === undefined ? getDefaultWasmModule() : compileModule(options.wasm);
    const channel = new WasmChannel();

    // The imports are built before the instance they read from exists, so the
    // memory is reached through the holder rather than captured.
    let instance: WasmInstance | undefined;
    instance = new WebAssembly.Instance(module, {
        wasi_snapshot_preview1: createWasiImports({ ...options.wasi, getMemory: () => instance!.exports.memory }),
        ts_host: channel.hostImports,
    }) as WasmInstance;
    // Reactor: run package initialization without invoking a `main`.
    instance.exports._initialize();

    channel.bind(instance.exports, {
        cwd: options.cwd ?? "/",
        ...(options.defaultLibraryPath !== undefined ? { defaultLibraryPath: options.defaultLibraryPath } : {}),
        ...(options.useCaseSensitiveFileNames !== undefined ? { useCaseSensitiveFileNames: options.useCaseSensitiveFileNames } : {}),
        ...(options.resolveModuleName !== undefined ? { resolveModuleName: true } : {}),
    });

    return new API({
        channel,
        fs: options.fs,
        resolveModuleName: options.resolveModuleName,
        collectTiming: options.collectTiming,
    } as unknown as APIOptions);
}

/**
 * Supplies the module every later {@link createWasmAPI} call instantiates.
 *
 * Compiling the reactor costs tens of milliseconds and tens of megabytes, and
 * instantiating it costs a few milliseconds, so it is compiled once. This is
 * also the only way to use the reactor where it cannot be read from disk: a
 * browser fetches and compiles it, then hands the result over here.
 */
export function setDefaultWasmModule(wasm: WasmSource): void {
    defaultModule = compileModule(wasm);
}

/** Whether a default module is already available without reading anything. */
export function hasDefaultWasmModule(): boolean {
    return defaultModule !== undefined;
}

/**
 * The module every {@link createWasmAPI} call without a `wasm` option
 * instantiates, reading and compiling it from disk on first use.
 */
export function getDefaultWasmModule(): WebAssembly.Module {
    return defaultModule ??= compileModule(readDefaultWasmBytes());
}

/** A reactor instance, with the reactor's own exports named. */
interface WasmInstance {
    readonly exports: WasmExports & { _initialize(): void; };
}

let defaultModule: WebAssembly.Module | undefined;

function compileModule(wasm: WasmSource): WebAssembly.Module {
    return wasm instanceof Uint8Array || wasm instanceof ArrayBuffer ? new WebAssembly.Module(wasm) : wasm;
}

/**
 * Reads `typescript.wasm` from beside this module.
 *
 * Two locations, because this module is shipped both as itself and inlined into
 * a bundle: the copy in this package's `dist`, and a copy placed next to
 * whatever bundle inlined it — which is what a bundler that rewrote
 * `import.meta.url` will find.
 */
function readDefaultWasmBytes(): Uint8Array {
    const read = findSyncFileReader();
    if (read === undefined) {
        throw new Error(
            "The TypeScript compiler could not be read from disk. In a browser it has to be compiled first: "
                + "`await initializeWasm()` before creating a Project, and run ts-morph in a Web Worker.",
        );
    }
    let firstError: unknown;
    for (const candidate of ["./typescript.wasm", "../../../dist/typescript.wasm"]) {
        try {
            return read(new URL(candidate, import.meta.url));
        }
        catch (error) {
            firstError ??= error;
        }
    }
    throw firstError;
}

/**
 * A synchronous file reader from whatever the host provides without an import:
 * Deno's global, the `require` a CommonJS bundle is handed, or the built-in
 * module registry an ESM Node process has.
 *
 * An import would be worse in three ways at once: `node:fs` cannot appear in a
 * browser bundle at all, the Deno build strips its single `node:fs` import
 * declaration on the way out, and this module is the one place in the package
 * that has to work in all three.
 */
function findSyncFileReader(): ((url: URL) => Uint8Array) | undefined {
    const deno = (globalThis as { Deno?: { readFileSync?(path: URL): Uint8Array; }; }).Deno;
    if (typeof deno?.readFileSync === "function") {
        return url => deno.readFileSync!(url);
    }
    const nodeFs = requireBuiltin("fs") ?? getBuiltinModule("node:fs");
    const readFileSync = (nodeFs as { readFileSync?(path: URL): Uint8Array; } | undefined)?.readFileSync;
    return typeof readFileSync === "function" ? url => readFileSync(url) : undefined;
}

function requireBuiltin(name: string): unknown {
    // `require` exists only where a CommonJS bundle provides it; `typeof` on an
    // undeclared name is safe everywhere else.
    if (typeof require !== "function") {
        return undefined;
    }
    try {
        // The specifier is assembled rather than written out so that a bundler
        // targeting the browser does not try to resolve it — the same trick
        // rollup's own CommonJS shims use.
        return require("node" + ":" + name);
    }
    catch {
        // a bundler's `require` may refuse a built-in outright
        return undefined;
    }
}

/** The built-in module registry an ESM Node process has, and Deno with it. */
function getBuiltinModule(id: string): unknown {
    return (globalThis as { process?: { getBuiltinModule?(id: string): unknown; }; }).process?.getBuiltinModule?.(id);
}
