// Browser build of getExePath.
//
// Locating a native binary means reading the filesystem and knowing the
// platform, neither of which a browser has. This stand-in keeps the sync
// client bundleable for the browser without pulling node:fs / node:path in:
// it throws, which the availability check in create.ts catches and treats as
// "no native build here", falling back to the WebAssembly reactor.
export default function getExePath() {
    throw new Error("No native TypeScript binary is available in the browser; use the WebAssembly reactor.");
}
