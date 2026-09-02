/**
 * In-process WebAssembly transport for the API client.
 *
 * The tsgo-wasm reactor module (see cmd/tsgo-wasm) runs the API server in the
 * same process as the client. This channel drives it synchronously and exposes
 * the same surface as the subprocess-backed {@link SyncRpcChannel}, so the two
 * are interchangeable behind {@link Client}. Filesystem access is delegated back
 * to JS through the host imports returned by {@link WasmChannel.hostImports},
 * reusing the same callback contract as the STDIO server.
 */

/**
 * A synchronous request channel to the API server. Implemented by both the
 * subprocess-backed SyncRpcChannel and the in-process {@link WasmChannel}.
 */
export interface RpcChannel {
    lastBytesSent: number;
    lastBytesReceived: number;
    requestSync(method: string, payload: string): string;
    requestBinarySync(method: string, payload: Uint8Array): Uint8Array;
    registerCallback(name: string, callback: (name: string, payload: string) => string): void;
    close(): void;
}

/**
 * The subset of a WebAssembly memory this channel uses. Declared structurally
 * so the package does not need the DOM library for its types.
 */
/** What the reactor needs to build its session. */
export interface SessionOptions {
    /** Current working directory used for module resolution. */
    cwd: string;
    /**
     * Whether the host answers module resolutions itself. Off by default: with
     * it on the compiler asks the host about every specifier.
     */
    resolveModuleName?: boolean;
    /**
     * Directory the default lib files are read from. Defaults to the libs bundled
     * in the module; a host that supplies its own lib files points this at them.
     */
    defaultLibraryPath?: string;
    /**
     * Whether the host's file system distinguishes case. Defaults to true; a host
     * backed by a Windows or macOS disk says false so paths resolve the way that
     * disk resolves them.
     */
    useCaseSensitiveFileNames?: boolean;
}

export interface WasmMemory {
    buffer: ArrayBuffer;
    /** Grows the memory by `delta` 64 KiB pages, returning the previous size in pages; throws when the host refuses. */
    grow(delta: number): number;
}

/** The exports of the tsgo-wasm reactor module. */
export interface WasmExports {
    memory: WasmMemory;
    create_session(cwdPtr: number, cwdLen: number): number;
    close_session(): void;
    get_request_buffer(size: number): number;
    handle_request(methodLen: number, payloadLen: number): number;
    response_ptr(): number;
    response_len(): number;
    response_is_binary(): number;
}

/** The `ts_host` import module the reactor expects. */
export interface WasmHostImports {
    callback(namePtr: number, nameLen: number, argPtr: number, argLen: number): number;
    read_result(destPtr: number, destLen: number): void;
}

const EMPTY = new Uint8Array(0);

/**
 * Marks a callback's return value as an error length rather than a result
 * length. Must match `hostErrorFlag` in cmd/tsgo-wasm/main.go.
 */
const HOST_ERROR_FLAG = 1 << 31;

export class WasmChannel implements RpcChannel {
    lastBytesSent = 0;
    lastBytesReceived = 0;

    /**
     * Whether the most recent response was raw binary rather than JSON, as
     * reported by the module's `response_is_binary` export.
     */
    lastResponseIsBinary = false;

    private exports: WasmExports | undefined;
    private readonly callbacks = new Map<string, (name: string, payload: string) => string>();
    private readonly encoder = new TextEncoder();
    private readonly decoder = new TextDecoder();
    private callbackResult = EMPTY;
    private inRequest = false;

    /**
     * The `ts_host` imports bound to this channel. Pass these to the module's
     * import object. They are only invoked while a request is in flight (i.e.
     * after {@link bind}), so it is safe to read them before binding.
     *
     * Neither import may throw. A JS exception thrown out of a wasm import
     * unwinds the Go frames as a trap rather than a Go panic, so no deferred
     * unlock runs, any goroutine waiting on the trapped one deadlocks, and the
     * module is permanently dead. Failure is reported through the return value
     * instead, which the Go side turns into an ordinary error.
     */
    readonly hostImports: WasmHostImports = {
        callback: (namePtr, nameLen, argPtr, argLen) => {
            try {
                const name = this.readString(namePtr, nameLen);
                const arg = this.readString(argPtr, argLen);
                const callback = this.callbacks.get(name);
                const result = callback ? callback(name, arg) : "";
                this.callbackResult = result.length ? this.encoder.encode(result) : EMPTY;
                return this.callbackResult.length;
            }
            catch (error) {
                this.callbackResult = this.encoder.encode(describeError(error));
                return HOST_ERROR_FLAG | this.callbackResult.length;
            }
        },
        read_result: (destPtr, destLen) => {
            try {
                const result = this.callbackResult;
                // Clear first so a protocol desync copies nothing rather than
                // stale bytes from an earlier callback.
                this.callbackResult = EMPTY;
                const length = Math.min(destLen, result.length);
                if (length > 0) {
                    this.view(destPtr, length).set(result.subarray(0, length));
                }
            }
            catch {
                // Nowhere to report to: read_result has no return value, and
                // throwing would kill the module. Go sees zeroed bytes and fails
                // to decode them, which surfaces as a request error.
            }
        },
    };

    /**
     * Binds an instantiated reactor and creates the API session. Must be called
     * exactly once, before any request.
     */
    bind(exports: WasmExports, options: SessionOptions): void {
        this.exports = exports;
        const cwdBytes = this.encoder.encode(JSON.stringify(options));
        const ptr = exports.get_request_buffer(cwdBytes.length || 1);
        if (cwdBytes.length) {
            this.view(ptr, cwdBytes.length).set(cwdBytes);
        }
        if (exports.create_session(ptr, cwdBytes.length) !== 0) {
            const message = this.decoder.decode(this.view(exports.response_ptr(), exports.response_len()));
            this.exports = undefined;
            throw new Error(message || "tsgo-wasm create_session failed");
        }
    }

    registerCallback(name: string, callback: (name: string, payload: string) => string): void {
        this.callbacks.set(name, callback);
    }

    requestSync(method: string, payload: string): string {
        const response = this.request(method, payload ? this.encoder.encode(payload) : EMPTY);
        return response.length ? this.decoder.decode(response) : "";
    }

    requestBinarySync(method: string, payload: Uint8Array): Uint8Array {
        return this.request(method, payload);
    }

    close(): void {
        const exports = this.exports;
        this.exports = undefined;
        this.callbacks.clear();
        this.callbackResult = EMPTY;
        if (exports && !this.inRequest) {
            exports.close_session();
        }
    }

    private request(method: string, payload: Uint8Array): Uint8Array {
        const exports = this.exports;
        if (!exports) {
            throw new Error("WasmChannel is not bound to a module");
        }
        // The request buffer, the response buffer and the stashed callback
        // result are all single-slot. Re-entering from inside a filesystem
        // callback would clobber the in-flight request and deadlock the module's
        // callback mutex, so refuse rather than corrupt.
        if (this.inRequest) {
            throw new Error(`WasmChannel: re-entrant request for "${method}" while another request is in flight`);
        }
        this.inRequest = true;
        try {
            const methodBytes = this.encoder.encode(method);
            const size = methodBytes.length + payload.length;
            const ptr = exports.get_request_buffer(size || 1);
            const buffer = this.view(ptr, size);
            buffer.set(methodBytes, 0);
            buffer.set(payload, methodBytes.length);
            this.lastBytesSent = size;

            const status = exports.handle_request(methodBytes.length, payload.length);
            const response = this.view(exports.response_ptr(), exports.response_len()).slice();
            this.lastBytesReceived = response.length;
            this.lastResponseIsBinary = exports.response_is_binary() !== 0;
            if (status !== 0) {
                throw new Error(this.decoder.decode(response) || "tsgo-wasm request failed");
            }
            return response;
        }
        finally {
            this.inRequest = false;
        }
    }

    /**
     * A view over `len` bytes of linear memory at `ptr`. Always reads
     * `memory.buffer` afresh because it is detached whenever the module grows
     * its memory.
     */
    private view(ptr: number, len: number): Uint8Array {
        return new Uint8Array(this.exports!.memory.buffer, ptr, len);
    }

    private readString(ptr: number, len: number): string {
        return this.decoder.decode(this.view(ptr, len));
    }
}

/** A message for anything that can be thrown, including non-Error values. */
function describeError(error: unknown): string {
    if (error instanceof Error) {
        return error.stack ? `${error.message}\n${error.stack}` : error.message;
    }
    try {
        return String(error);
    }
    catch {
        return "unknown host callback error";
    }
}
