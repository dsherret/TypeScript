/**
 * Browser build of {@link SyncRpcChannel}.
 *
 * The real one spawns a child process and blocks on pipe file descriptors, which
 * a browser has neither of. This stand-in exists only so the sync client can be
 * bundled for the browser without dragging node:child_process / node:fs in: it
 * satisfies the same shape but refuses to be constructed. Reaching it means the
 * native backend was selected where it cannot run, which the auto-selecting
 * entry point (see create.ts) is meant to prevent — so the message points there.
 */

import type { RpcChannel } from "./wasmChannel.ts";

export class SyncRpcChannel implements RpcChannel {
    lastBytesSent = 0;
    lastBytesReceived = 0;

    constructor(_exe: string, _args: string[], _collectTiming = false) {
        throw new Error(
            "The native subprocess backend is not available in the browser. "
                + "Use the WebAssembly reactor instead — createAPI() falls back to it "
                + "automatically, or call createWasmAPI() directly.",
        );
    }

    requestSync(_method: string, _payload: string): string {
        throw new Error("SyncRpcChannel is unavailable in the browser.");
    }

    requestBinarySync(_method: string, _payload: Uint8Array): Uint8Array {
        throw new Error("SyncRpcChannel is unavailable in the browser.");
    }

    registerCallback(_name: string, _callback: (name: string, payload: string) => string): void {
        throw new Error("SyncRpcChannel is unavailable in the browser.");
    }

    close(): void {
        // nothing was ever opened
    }
}
