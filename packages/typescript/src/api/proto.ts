import type { TextEdit } from "./proto.generated.ts";
import {
    documentURIToFileName,
    fileNameToDocumentURI,
} from "./path.ts";
import type {
    APIMethodInfo,
    DocumentIdentifier,
    SignatureResponse,
    SourceFileResponse,
    SymbolResponse,
    TypeResponse,
    UpdateSnapshotParams as CoreUpdateSnapshotParams,
} from "./proto.generated.ts";
export type { ConfigFileResponse as ParsedCommandLine, DiagnosticResponse as Diagnostic } from "./proto.generated.ts";

export * from "./proto.generated.ts";

export type APIMethodsReturning<T> = { [K in keyof APIMethodInfo]: [T] extends [NonNullable<APIMethodInfo[K]["result"]>] ? [NonNullable<APIMethodInfo[K]["result"]>] extends [T] ? K : never : never; }[keyof APIMethodInfo];

export type SourceFileResponseMethod = APIMethodsReturning<SourceFileResponse>;
export type SymbolPropertyMethod = APIMethodsReturning<SymbolResponse>;
export type SymbolsPropertyMethod = APIMethodsReturning<SymbolResponse[]>;
export type SignaturePropertyMethod = APIMethodsReturning<SignatureResponse>;
export type TypePropertyMethod = Exclude<APIMethodsReturning<TypeResponse>, IntrinsicTypeMethod>;
export type TypesPropertyMethod = APIMethodsReturning<TypeResponse[]>;
export type IntrinsicTypeMethod = "getAnyType" | "getBigIntType" | "getBooleanType" | "getESSymbolType" | "getNeverType" | "getNonPrimitiveType" | "getNullType" | "getNumberType" | "getStringType" | "getUndefinedType" | "getUnknownType" | "getVoidType";

/**
 * A position within a document, combining a document identifier with an offset.
 */
export interface DocumentPosition {
    /** The document containing the position */
    document: DocumentIdentifier;
    /** The character offset within the document */
    position: number;
}

/**
 * Resolves a DocumentIdentifier to a file name.
 * If the identifier contains a URI, it is converted to a file name.
 */
export function resolveFileName(identifier: DocumentIdentifier): string {
    if (typeof identifier === "string") {
        return identifier;
    }
    return documentURIToFileName(identifier.uri);
}

/**
 * Resolves a DocumentIdentifier to a document URI.
 * If the identifier contains a file name, it is converted to a URI.
 */
export function resolveDocumentURI(identifier: DocumentIdentifier): string {
    if (typeof identifier === "string") {
        return fileNameToDocumentURI(identifier);
    }
    return identifier.uri;
}

export interface LSPUpdateSnapshotParams extends CoreUpdateSnapshotParams {
    /**
     * @deprecated Use {@link openProjects} instead.
     * Path to a tsconfig.json file to open in the new snapshot.
     */
    openProject?: string;

    /** FileChanges are not supplied by the LSP */
    fileChanges?: never;
}

/**
 * Parameters for updateSnapshot, including deprecated members handled by `toUpdateSnapshotRequest`
 */
export interface UpdateSnapshotParams extends CoreUpdateSnapshotParams {
    /**
     * @deprecated Use {@link openProjects} instead.
     * Path to a tsconfig.json file to open in the new snapshot.
     */
    openProject?: string;
}

/**
 * Builds the wire request for updateSnapshot, applying the deprecated `openProject`
 * compatibility shim: a single `openProject` is folded into `openProjects` and is
 * never sent on the wire.
 */
export function toUpdateSnapshotRequest(params?: UpdateSnapshotParams): UpdateSnapshotParams {
    const { openProject, openProjects, ...rest } = params ?? {};
    const mergedOpenProjects = openProject !== undefined
        ? [resolveFileName(openProject), ...(openProjects ?? [])]
        : openProjects;
    return {
        ...rest,
        ...(mergedOpenProjects !== undefined ? { openProjects: mergedOpenProjects } : {}),
    };
}


/** Edits grouped by the file they apply to. */
export interface FileTextEdits {
    fileName: string;
    edits: TextEdit[];
}

/** A quick fix: a description plus the edits that apply it. */
export interface CodeFixAction {
    description: string;
    changes: FileTextEdits[];
}

/** The result of applying one fix id across a whole file. */
export interface CombinedCodeActions {
    description: string;
    changes: FileTextEdits[];
}

/** A span of a file, in character offsets. */
export interface FileSpan {
    fileName: string;
    pos: number;
    end: number;
}

/** Formatter settings. Unset fields fall back to the server's defaults. */
export interface FormattingOptions {
    tabSize?: number;
    insertSpaces?: boolean;
    trimTrailingWhitespace?: boolean;
    indentSize?: number;
    indentStyle?: number;
    newLineCharacter?: string;
}

export type OrganizeImportsMode = "all" | "sortAndCombine" | "removeUnused";

export type QuotePreference = "auto" | "double" | "single";

export interface ExportedSymbolResponse {
    name: string;
    declarations?: string[];
}
