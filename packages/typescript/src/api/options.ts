/**
 * Shared utilities for the TypeScript API client.
 */

import getExePath from "#getExePath";
import type { FileSystem } from "./fs.ts";
import type { RpcChannel } from "./wasmChannel.ts";

export interface ClientSocketOptions {
    /** Path to the Unix domain socket or Windows named pipe for API communication */
    pipe: string;
}

export interface ClientWasmOptions {
    /** A pre-built request channel bound to an in-process WebAssembly reactor. */
    channel: RpcChannel;
    /** Virtual filesystem callbacks */
    fs?: FileSystem;
    /** Resolves a module specifier in place of the compiler. */
    resolveModuleName?: ModuleNameResolver;
    /** When true, collect per-request timing information. */
    collectTiming?: boolean;
}

export interface ClientSpawnOptions {
    /** Path to the tsc executable. Defaults to the bundled tsc binary. */
    tsserverPath?: string;
    /** Current working directory */
    cwd?: string;
    /** Virtual filesystem callbacks */
    fs?: FileSystem;
    /** Resolves a module specifier in place of the compiler. */
    resolveModuleName?: ModuleNameResolver;
    /** Allow trusted projects to execute configured external content mapper processes. */
    runExternalCode?: boolean;
    /**
     * When true, collect timing information for each request. The client
     * measures round-trip latency and bytes sent/received, and the server
     * measures its own per-request processing time; both are combined (along
     * with an estimated transport overhead) in the snapshot returned by
     * {@link API.getTimingInfo}.
     */
    collectTiming?: boolean;
}

export type ClientOptions = ClientSocketOptions | ClientSpawnOptions | ClientWasmOptions;

export function isSpawnOptions(options: ClientOptions): options is ClientSpawnOptions {
    return !("pipe" in options) && !("channel" in options);
}

export function isWasmOptions(options: ClientOptions): options is ClientWasmOptions {
    return "channel" in options;
}

export function resolveExePath(options: ClientSpawnOptions): string {
    return options.tsserverPath ?? getExePath();
}

export function getAPIProcessArgs(options: ClientSpawnOptions, async: boolean): string[] {
    const args = ["--api"];
    if (async) args.push("--async");
    args.push("--cwd", options.cwd ?? process.cwd());
    if (options.runExternalCode) args.push("--runExternalCode");
    if (options.collectTiming) args.push("--timing");
    return args;
}

export interface LSPConnectionOptions extends ClientSocketOptions {
}

export interface APIOptions extends ClientSpawnOptions {
}

/** A request to resolve one module specifier. */
export interface ModuleNameResolutionRequest {
    moduleName: string;
    containingFile: string;
    resolutionMode: number;
}

/** Where a module specifier resolves to. */
export interface ResolvedModuleName {
    resolvedFileName: string;
    extension?: string;
    isExternalLibraryImport?: boolean;
    resolvedUsingTsExtension?: boolean;
}

export type ModuleNameResolver = (
    request: ModuleNameResolutionRequest,
) => ModuleNameResolution | undefined;

export type ModuleNameResolution =
    | { resolved: ResolvedModuleName | null; moduleName?: undefined; }
    | { moduleName: string; resolved?: undefined; };
