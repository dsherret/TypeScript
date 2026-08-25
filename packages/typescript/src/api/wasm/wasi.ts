/**
 * A `wasi_snapshot_preview1` implementation for the tsgo reactor, written
 * against the web platform only.
 *
 * The reactor is built with `GOOS=wasip1`, so the Go toolchain emits imports
 * against the WASI ABI whether or not the program uses them. Almost none of it
 * is used: the file system is delegated to JavaScript through the `ts_host`
 * callbacks, so the only calls the reactor makes are for the clocks, for
 * randomness, for the argument vector, and for a one-time probe of stdio and of
 * the preopened directory table. Answering those here instead of through Node's
 * own WASI implementation is what lets one loader serve Node, Deno and the
 * browser — nothing below touches a host built-in.
 *
 * Everything the reactor never calls still has to be present, or instantiation
 * fails on a missing import. Those report failure and go through
 * {@link WasiShimOptions.onUnsupported}, so a test can assert that a real
 * workload never reaches one.
 */

/** The subset of a WebAssembly memory this shim uses. */
export interface WasiMemory {
    buffer: ArrayBuffer;
}

export interface WasiShimOptions {
    /**
     * The reactor's linear memory. Consulted on every call rather than captured
     * once, because growing the memory detaches the previous `ArrayBuffer`.
     */
    getMemory(): WasiMemory;
    /** Receives text written to fd 1. Defaults to `console.log`. */
    onStdout?(text: string): void;
    /**
     * Receives text written to fd 2. Defaults to `console.error`.
     *
     * The Go runtime reports a fatal error here and then traps, so leaving this
     * silent turns a crash into an unexplained one.
     */
    onStderr?(text: string): void;
    /**
     * Called with the name of an import the shim does not implement, before it
     * reports failure. A working reactor never reaches one.
     */
    onUnsupported?(name: string): void;
}

/** The `wasi_snapshot_preview1` import object. */
export type WasiImports = Record<string, (...args: any[]) => number>;

/**
 * Builds the `wasi_snapshot_preview1` import object for one reactor instance.
 *
 * The instance does not exist yet when its imports are built, which is why the
 * memory arrives as a callback.
 */
export function createWasiImports(options: WasiShimOptions): WasiImports {
    const onStdout = options.onStdout ?? (text => console.log(text));
    const onStderr = options.onStderr ?? (text => console.error(text));
    const onUnsupported = options.onUnsupported ?? (() => {});
    // One decoder per stream, kept across calls: the runtime splits its output
    // at arbitrary byte offsets, so a multi-byte character can straddle two
    // iovecs or two writes. Decoding each piece on its own turns those into
    // replacement characters.
    const streams = {
        [stdoutFd]: { decoder: new TextDecoder("utf-8"), pending: "", emit: (text: string) => onStdout(text) },
        [stderrFd]: { decoder: new TextDecoder("utf-8"), pending: "", emit: (text: string) => onStderr(text) },
    } as Record<number, { decoder: TextDecoder; pending: string; emit: (text: string) => void; }>;

    /** Emits whole lines, holding the remainder until the writer completes it. */
    function writeToStream(fd: number, chunk: Uint8Array, final: boolean): void {
        const stream = streams[fd];
        stream.pending += stream.decoder.decode(chunk, { stream: !final });
        const lastBreak = stream.pending.lastIndexOf("\n");
        if (lastBreak >= 0) {
            stream.emit(stream.pending.slice(0, lastBreak));
            stream.pending = stream.pending.slice(lastBreak + 1);
        }
    }

    /**
     * Flushes what a writer left without a trailing newline.
     *
     * The Go runtime's fatal path ends in a trap rather than a final newline, so
     * the last line of a crash is only ever seen because of this.
     */
    function flushStreams(): void {
        for (const fd of [stdoutFd, stderrFd]) {
            const stream = streams[fd];
            stream.pending += stream.decoder.decode();
            if (stream.pending.length > 0) {
                stream.emit(stream.pending);
                stream.pending = "";
            }
        }
    }
    const view = () => new DataView(options.getMemory().buffer);
    const bytes = () => new Uint8Array(options.getMemory().buffer);

    /** Reports an import the reactor is not expected to call. */
    function unsupported(name: string, errno: number = errnoNosys): number {
        onUnsupported(name);
        return errno;
    }

    return {
        clock_time_get(clockId: number, _precision: bigint, timePtr: number): number {
            view().setBigUint64(timePtr, clockId === realtimeClockId ? realtimeNanoseconds() : monotonicNanoseconds(), true);
            return errnoSuccess;
        },

        random_get(bufPtr: number, length: number): number {
            const buffer = bytes().subarray(bufPtr, bufPtr + length);
            // `crypto.getRandomValues` rejects a request over 65536 bytes.
            for (let offset = 0; offset < length; offset += randomChunkSize) crypto.getRandomValues(buffer.subarray(offset, Math.min(offset + randomChunkSize, length)));
            return errnoSuccess;
        },

        args_sizes_get(countPtr: number, bufferSizePtr: number): number {
            const data = view();
            data.setUint32(countPtr, args.length, true);
            data.setUint32(bufferSizePtr, args.reduce((size, arg) => size + arg.length + 1, 0), true);
            return errnoSuccess;
        },

        args_get(argvPtr: number, bufferPtr: number): number {
            const data = view();
            const memory = bytes();
            let position = bufferPtr;
            for (let i = 0; i < args.length; i++) {
                data.setUint32(argvPtr + i * 4, position, true);
                for (let c = 0; c < args[i].length; c++) memory[position++] = args[i].charCodeAt(c);
                memory[position++] = 0;
            }
            return errnoSuccess;
        },

        /**
         * Reports an empty environment. That answer is load-bearing: it is what
         * keeps the reactor from ever calling `environ_get`.
         */
        environ_sizes_get(countPtr: number, bufferSizePtr: number): number {
            const data = view();
            data.setUint32(countPtr, environment.length, true);
            data.setUint32(bufferSizePtr, environment.reduce((size, entry) => size + entry.length + 1, 0), true);
            return errnoSuccess;
        },

        environ_get(environPtr: number, bufferPtr: number): number {
            const data = view();
            const memory = bytes();
            let position = bufferPtr;
            for (let i = 0; i < environment.length; i++) {
                data.setUint32(environPtr + i * 4, position, true);
                for (let c = 0; c < environment[i].length; c++) memory[position++] = environment[i].charCodeAt(c);
                memory[position++] = 0;
            }
            return errnoSuccess;
        },

        /**
         * Answers every subscription as already ready.
         *
         * Go's netpoll treats a non-zero result as fatal, so this has to succeed
         * even though there is nothing to wait on. Every observed call is a
         * single relative monotonic clock subscription with a zero timeout — a
         * yield — and a single-threaded host has nothing to yield to.
         */
        poll_oneoff(subscriptionsPtr: number, eventsPtr: number, subscriptionCount: number, eventCountPtr: number): number {
            const data = view();
            for (let i = 0; i < subscriptionCount; i++) {
                const subscription = subscriptionsPtr + i * subscriptionSize;
                const event = eventsPtr + i * eventSize;
                data.setBigUint64(event, data.getBigUint64(subscription, true), true); // userdata, echoed back
                data.setUint16(event + 8, errnoSuccess, true);
                data.setUint8(event + 10, data.getUint8(subscription + 8)); // event type, echoed back
                data.setBigUint64(event + 16, 0n, true); // nbytes
                data.setUint16(event + 24, 0, true); // flags
            }
            data.setUint32(eventCountPtr, subscriptionCount, true);
            return errnoSuccess;
        },

        /**
         * Diagnostics only.
         *
         * File data never comes through here — it goes over the `ts_host`
         * callbacks — so the only thing that ever reaches this is the Go
         * runtime's fatal error path, which arrives as several hundred small
         * writes to fd 2 just before the module traps.
         */
        fd_write(fd: number, iovsPtr: number, iovsLength: number, writtenPtr: number): number {
            if (fd !== stdoutFd && fd !== stderrFd) return unsupported("fd_write", errnoBadf);
            const data = view();
            const memory = bytes();
            const chunks: Uint8Array[] = [];
            let written = 0;
            for (let i = 0; i < iovsLength; i++) {
                const base = data.getUint32(iovsPtr + i * 8, true);
                const length = data.getUint32(iovsPtr + i * 8 + 4, true);
                if (length > 0) chunks.push(memory.slice(base, base + length));
                written += length;
            }
            for (let i = 0; i < chunks.length; i++) writeToStream(fd, chunks[i], false);
            data.setUint32(writtenPtr, written, true);
            return errnoSuccess;
        },

        /** Describes stdio as a character device; the reactor probes each of fd 0-2 at startup. */
        fd_fdstat_get(fd: number, statPtr: number): number {
            if (fd > stderrFd) return errnoBadf;
            bytes().fill(0, statPtr, statPtr + fdstatSize);
            view().setUint8(statPtr, filetypeCharacterDevice);
            return errnoSuccess;
        },

        fd_fdstat_set_flags(): number {
            return errnoSuccess;
        },

        /**
         * Reports that there are no preopened directories.
         *
         * `EBADF` rather than `ENOSYS` is deliberate: it is how Go's runtime
         * learns the table has ended, and it is what stops it before
         * `fd_prestat_dir_name`.
         */
        fd_prestat_get(): number {
            return errnoBadf;
        },

        // The reactor reaches none of the following. They exist because a
        // missing import makes the module refuse to instantiate.
        fd_close: () => unsupported("fd_close", errnoBadf),
        fd_read: () => unsupported("fd_read", errnoBadf),
        fd_pread: () => unsupported("fd_pread", errnoBadf),
        fd_readdir: () => unsupported("fd_readdir", errnoBadf),
        fd_filestat_get: () => unsupported("fd_filestat_get", errnoBadf),
        fd_prestat_dir_name: () => unsupported("fd_prestat_dir_name", errnoBadf),
        path_open: () => unsupported("path_open"),
        path_filestat_get: () => unsupported("path_filestat_get"),
        path_create_directory: () => unsupported("path_create_directory"),
        path_remove_directory: () => unsupported("path_remove_directory"),
        path_unlink_file: () => unsupported("path_unlink_file"),
        sched_yield: () => unsupported("sched_yield", errnoSuccess),
        proc_exit(code: number): number {
            onUnsupported("proc_exit");
            flushStreams();
            // Reporting an errno would let the reactor carry on in a state where
            // its runtime believes the process has ended.
            throw new Error(`The tsgo reactor called proc_exit(${code}).`);
        },
    };
}

const errnoSuccess = 0;
const errnoBadf = 8;
const errnoNosys = 52;

const stdoutFd = 1;
const stderrFd = 2;
const filetypeCharacterDevice = 2;
const fdstatSize = 24;
const subscriptionSize = 48;
const eventSize = 32;
const randomChunkSize = 65536;
const realtimeClockId = 0;
const nanosecondsPerMillisecond = 1000000;

/** The argument vector the reactor sees, matching what Node's WASI was given. */
const args = ["tsgo-wasm"];

/**
 * The environment the reactor sees.
 *
 * Only the Go runtime reads this — nothing in the compiler asks for a variable — so it
 * carries collector settings and nothing else.
 *
 * `GOGC` is here because the reactor was running at the default of 100, collecting every
 * time the heap doubled. Encoding a syntax tree allocates heavily and briefly, which is
 * the shape that setting suits worst: profiling a 200-file run put `runtime.growMemory`
 * at the top of the whole profile, with the collector's scan and write-barrier frames
 * behind it. Growing the heap is not free here the way it is natively — it is a
 * `memory.grow` on the module's linear memory, which the host may satisfy by moving it.
 */
const environment = ["GOGC=400"];

function realtimeNanoseconds() {
    // Date.now() is milliseconds, and the reactor asks for nanoseconds; pairing
    // the time origin with the monotonic clock keeps the sub-millisecond part
    // that node:wasi reported.
    return BigInt(Math.round((performance.timeOrigin + performance.now()) * 1e6));
}

function monotonicNanoseconds(): bigint {
    const nanoseconds = BigInt(Math.round(performance.now() * nanosecondsPerMillisecond));
    // Go's runtime aborts with "nanotime returning zero" if the monotonic clock
    // ever reads 0, which `performance.now()` legitimately can on its first call.
    return nanoseconds === 0n ? 1n : nanoseconds;
}
