import {
    type FileReference,
    forEachLeadingCommentRange,
    forEachTrailingCommentRange,
    getChildren,
    getFirstToken,
    getLastToken,
    ModifierFlags,
    type Node,
    type SourceFile,
    SyntaxKind,
} from "../../ast/index.ts";
import type { TimingCollector } from "../timing.ts";
import {
    HEADER_OFFSET_HASH_HI0,
    HEADER_OFFSET_HASH_HI1,
    HEADER_OFFSET_HASH_LO0,
    HEADER_OFFSET_HASH_LO1,
    HEADER_OFFSET_PARSE_OPTIONS,
    NODE_DATA_TYPE_CHILDREN,
    NODE_DATA_TYPE_EXTENDED,
    NODE_DATA_TYPE_STRING,
    NODE_OFFSET_DATA,
    NODE_OFFSET_END,
    NODE_OFFSET_KIND,
    NODE_OFFSET_NEXT,
    NODE_OFFSET_PARENT,
    NODE_OFFSET_POS,
} from "./protocol.ts";

// ═══════════════════════════════════════════════════════════════════════════
// Constants
// ═══════════════════════════════════════════════════════════════════════════

export const popcount8: number[] = [0, 1, 1, 2, 1, 2, 2, 3, 1, 2, 2, 3, 2, 3, 3, 4, 1, 2, 2, 3, 2, 3, 3, 4, 2, 3, 3, 4, 3, 4, 4, 5, 1, 2, 2, 3, 2, 3, 3, 4, 2, 3, 3, 4, 3, 4, 4, 5, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 1, 2, 2, 3, 2, 3, 3, 4, 2, 3, 3, 4, 3, 4, 4, 5, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 3, 4, 4, 5, 4, 5, 5, 6, 4, 5, 5, 6, 5, 6, 6, 7, 1, 2, 2, 3, 2, 3, 3, 4, 2, 3, 3, 4, 3, 4, 4, 5, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 3, 4, 4, 5, 4, 5, 5, 6, 4, 5, 5, 6, 5, 6, 6, 7, 2, 3, 3, 4, 3, 4, 4, 5, 3, 4, 4, 5, 4, 5, 5, 6, 3, 4, 4, 5, 4, 5, 5, 6, 4, 5, 5, 6, 5, 6, 6, 7, 3, 4, 4, 5, 4, 5, 5, 6, 4, 5, 5, 6, 5, 6, 6, 7, 4, 5, 5, 6, 5, 6, 6, 7, 5, 6, 6, 7, 6, 7, 7, 8];

export type NodeDataType = typeof NODE_DATA_TYPE_CHILDREN | typeof NODE_DATA_TYPE_STRING | typeof NODE_DATA_TYPE_EXTENDED;
export const NODE_DATA_TYPE_MASK = 0xc0_00_00_00;
export const NODE_CHILD_MASK = 0x00_00_00_ff;
export const NODE_STRING_INDEX_MASK = 0x00_ff_ff_ff;
export const NODE_EXTENDED_DATA_MASK = 0x00_ff_ff_ff;

// ═══════════════════════════════════════════════════════════════════════════
// SourceFileInfo — the interface RemoteNode/RemoteNodeList need from the
// source file, avoiding a direct dependency on RemoteSourceFile.
// ═══════════════════════════════════════════════════════════════════════════

// The global type is not available in earlier @types/node versions
export interface TextDecoder {
    decode(input?: ArrayBufferView | ArrayBufferLike): string;
}

export interface SourceFileInfo {
    readonly _offsetNodes: number;
    readonly _offsetStringTableOffsets: number;
    readonly _offsetStringTable: number;
    readonly _offsetExtendedData: number;
    readonly _offsetStructuredData: number;
    readonly _decoder: TextDecoder;
    readonly text: string;
    nodes: any[];
    readonly path?: string;
    /**
     * The timing collector that per-node materialization is reported into, and
     * that this source file registered itself with when fetched. Present only
     * when timing collection is enabled; when undefined, materialization is not
     * timed and no clock is read.
     */
    readonly _timing?: TimingCollector | undefined;
    readFileReferences(offset: number): readonly FileReference[];
    readNodeIndexArray(offset: number): readonly Node[];
    readStringArray(offset: number): readonly string[];
    getOrCreateNodeAtIndex(index: number): Node;
}

// ═══════════════════════════════════════════════════════════════════════════
// Free functions
// ═══════════════════════════════════════════════════════════════════════════

/**
 * Read the 128-bit content hash from a source file binary response as a hex string.
 */
export function readSourceFileHash(data: DataView): string {
    const lo0 = data.getUint32(HEADER_OFFSET_HASH_LO0, true);
    const lo1 = data.getUint32(HEADER_OFFSET_HASH_LO1, true);
    const hi0 = data.getUint32(HEADER_OFFSET_HASH_HI0, true);
    const hi1 = data.getUint32(HEADER_OFFSET_HASH_HI1, true);
    return hex8(hi1) + hex8(hi0) + hex8(lo1) + hex8(lo0);
}

/**
 * Read the per-file parse options key from a source file binary response.
 * This encodes the ExternalModuleIndicatorOptions bitmask as a string,
 * allowing the client to distinguish files parsed with different options.
 */
export function readParseOptionsKey(data: DataView): string {
    return data.getUint32(HEADER_OFFSET_PARSE_OPTIONS, true).toString();
}

function hex8(n: number): string {
    return (n >>> 0).toString(16).padStart(8, "0");
}

export function modifierToFlag(kind: SyntaxKind): ModifierFlags {
    switch (kind) {
        case SyntaxKind.StaticKeyword:
            return ModifierFlags.Static;
        case SyntaxKind.PublicKeyword:
            return ModifierFlags.Public;
        case SyntaxKind.ProtectedKeyword:
            return ModifierFlags.Protected;
        case SyntaxKind.PrivateKeyword:
            return ModifierFlags.Private;
        case SyntaxKind.AbstractKeyword:
            return ModifierFlags.Abstract;
        case SyntaxKind.AccessorKeyword:
            return ModifierFlags.Accessor;
        case SyntaxKind.ExportKeyword:
            return ModifierFlags.Export;
        case SyntaxKind.DeclareKeyword:
            return ModifierFlags.Ambient;
        case SyntaxKind.ConstKeyword:
            return ModifierFlags.Const;
        case SyntaxKind.DefaultKeyword:
            return ModifierFlags.Default;
        case SyntaxKind.AsyncKeyword:
            return ModifierFlags.Async;
        case SyntaxKind.ReadonlyKeyword:
            return ModifierFlags.Readonly;
        case SyntaxKind.OverrideKeyword:
            return ModifierFlags.Override;
        case SyntaxKind.InKeyword:
            return ModifierFlags.In;
        case SyntaxKind.OutKeyword:
            return ModifierFlags.Out;
        case SyntaxKind.Decorator:
            return ModifierFlags.Decorator;
        default:
            return ModifierFlags.None;
    }
}

/**
 * The position of the `/**` that opens the doc comment ending at `end`.
 *
 * The compiler gives a `JSDoc` node its full start, so whitespace and any
 * comment that is not the doc comment itself — a `//` line comment closing the
 * previous line, a plain `/* *\/` block, a shebang — fall inside `[pos, end)`.
 * Classic TypeScript starts the node at its `/**` instead, and callers that read
 * a doc comment's text or its start expect that, so the leading trivia is
 * skipped here.
 *
 * The trivia is walked with the scanner's own comment iteration, trailing ranges
 * before leading ones, which is how `parser.GetJSDocCommentRanges` found these
 * comments in the first place: a parameter's or an arrow function's doc comment
 * follows its `pos` on the same line and is a trailing range, everything else is
 * a leading one. Returns `pos` unchanged when no comment ends where the node
 * does, which is the case for a doc comment the parser synthesized out of a tag
 * rather than read from the file.
 */
function getDocCommentStart(text: string, pos: number, end: number): number {
    let start = pos;
    // The iteration stops on the first truthy result, so the answer is written
    // out to `start` rather than returned: a doc comment at the start of the
    // file is at position 0, which would not stop it.
    const found = (commentPos: number, commentEnd: number): true | undefined => {
        if (commentEnd !== end) {
            return undefined;
        }
        start = commentPos;
        return true;
    };
    if (forEachTrailingCommentRange(text, pos, found) === undefined) {
        forEachLeadingCommentRange(text, pos, found);
    }
    return start;
}

// ═══════════════════════════════════════════════════════════════════════════
// RemoteNodeBase
// ═══════════════════════════════════════════════════════════════════════════

export class RemoteNodeBase {
    parent: any; // RemoteNode at runtime
    view: DataView;
    protected index: number;
    protected _byteIndex: number;
    /** Memo for `pos` on a `JSDoc` node, whose start has to be scanned out of the node's leading trivia. */
    private _docCommentPos: number | undefined;

    constructor(view: DataView, index: number, parent: any, byteIndex: number) {
        this.view = view;
        this.index = index;
        this.parent = parent;
        this._byteIndex = byteIndex;
    }

    get kind(): SyntaxKind {
        return this.view.getUint32(this._byteIndex + NODE_OFFSET_KIND, true);
    }

    /**
     * Returns every child in source order, including the tokens and
     * `SyntaxList` nodes the tree does not store.
     *
     * The free function does the work; it lives in ../../ast/children.ts
     * because it is about the AST rather than about the wire format. `this` is
     * always a RemoteNode at runtime — only that subclass is constructed —
     * which is why getSourceFile is reachable here.
     */
    getChildren(sourceFile?: SourceFile): Node[] {
        const node = this as unknown as Node;
        return getChildren(node, sourceFile ?? node.getSourceFile());
    }

    getChildCount(sourceFile?: SourceFile): number {
        return this.getChildren(sourceFile).length;
    }

    getChildAt(index: number, sourceFile?: SourceFile): Node {
        return this.getChildren(sourceFile)[index];
    }

    getFirstToken(sourceFile?: SourceFile): Node | undefined {
        const node = this as unknown as Node;
        return getFirstToken(node, sourceFile ?? node.getSourceFile());
    }

    getLastToken(sourceFile?: SourceFile): Node | undefined {
        const node = this as unknown as Node;
        return getLastToken(node, sourceFile ?? node.getSourceFile());
    }

    get pos(): number {
        const pos = this.view.getInt32(this._byteIndex + NODE_OFFSET_POS, true);
        if (this.kind !== SyntaxKind.JSDoc) {
            return pos;
        }
        return this._docCommentPos ??= getDocCommentStart(this.sourceFile.text, pos, this.end);
    }

    get end(): number {
        return this.view.getInt32(this._byteIndex + NODE_OFFSET_END, true);
    }

    get next(): number {
        return this.view.getUint32(this._byteIndex + NODE_OFFSET_NEXT, true);
    }

    protected get parentIndex(): number {
        return this.view.getUint32(this._byteIndex + NODE_OFFSET_PARENT, true);
    }

    protected get data(): number {
        return this.view.getUint32(this._byteIndex + NODE_OFFSET_DATA, true);
    }

    protected get dataType(): NodeDataType {
        return (this.data & NODE_DATA_TYPE_MASK) as NodeDataType;
    }

    protected get childMask(): number {
        if (this.dataType !== NODE_DATA_TYPE_CHILDREN) {
            return -1;
        }
        return this.data & NODE_CHILD_MASK;
    }

    protected getFileText(start: number, end: number): string {
        return this.sourceFile._decoder.decode(new Uint8Array(this.view.buffer, this.view.byteOffset + this.sourceFile._offsetStringTable + start, end - start));
    }

    protected get sourceFile(): SourceFileInfo {
        // Overridden in RemoteNode; exists here for getFileText access
        throw new Error("sourceFile not available on base");
    }
}
