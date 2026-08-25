# Porting the ts-morph fork onto microsoft/TypeScript main

Branch `migrate-tsmain`, based on `microsoft/TypeScript` main (`e533f4c083`).
The fork's 40-commit delta (from base `8d29e62f3`) is being re-applied onto the
restructured, rearchitected upstream.

## Path remap (done)

    internal/**                        -> tsc/internal/**
    cmd/**                             -> tsc/cmd/**
    _packages/native-preview/src/**    -> packages/typescript/src/**
    _packages/native-preview/test/**   -> packages/typescript/test/**

Every Go import was rewritten `github.com/microsoft/typescript-go/internal`
-> `github.com/microsoft/TypeScript/tsc/internal`.

## Go build status: 9 of 11 packages compile

OK: ast, core, tsoptions, checker, encoder, module, printer, compiler, ls
FAIL: project, api

### Reconciliations already done
- encoder.go: restored `strconv` import (body applied, import hunk rejected).
- tsoptions/parsedcommandline.go: dropped removed `extraFileExtensions` field from
  `WithAdditionalRootFiles`.
- checker/services.go: dropped `GetFullyQualifiedName` (now upstream in exports.go).
- compiler/emitter.go: dropped `WriteByteOrderMark` (upstream adds the BOM to the text).
- ls/rename.go: added `RenameOptions` type, threaded `options` through `ProvideRename`
  and the `getRenameInfoForNode` callers.
- ls/codeactions.go, languageservice.go: restored `lsutil` / `core` imports.
- ls/format.go: `l.converters.FromLSPRange(file, r)` -> `lsconv.FromLSPRangeToOriginal(l.converters, file, r)`.
- ls/jsdoc.go: `getDocumentationFromDeclaration` is a package func taking `noMappedLocation`.
- project imports: added `core`/`module` to autoimport.go, compilerhost.go.

### Deferred: incremental roots (~770 lines)
Reverted `compiler/{fileloader,program,filesparser,fileInclude,host}.go` and
`processingDiagnostic.go`, `includeprocessor.go` to upstream; removed the
`addrootfiles*`/`removerootfiles` tests. Upstream rewrote the file loader around
content-mappers + an include-processor, so the feature (`removedRoots`,
`processRootFileChanges`, `canRemoveRoots`, `Program.RootFileChangesFrom`,
`Program.UpdateRootFiles`, program-extension path) must be re-implemented against the
new architecture, not merged.

## The hard core (why project + api don't build)

These are one cohesive in-process layer that upstream's content-mapper rewrite cut
across. They cannot be merged piecemeal:

1. **Parse cache design conflict.** Our `refcountcache.go` adds a `CachedValue`
   interface (`HostCacheEntry`/`SetHostCacheEntry`) for value-based ref/deref. Upstream's
   `parsecache.go` / `ContentMappedParseCache` holds `contentmapper.SourceFiles`, which
   does not satisfy `CachedValue`. Reconcile the two cache designs (or drop the
   value-based ref and use key-based `Deref`).
2. **Incremental roots.** `project.go` calls `Program.RootFileChangesFrom` /
   `UpdateRootFiles` (deferred above). Re-implement or stub to a full-rebuild fallback.
3. **Module-resolution hook.** `SessionOptions.ResolveModuleName` and
   `callbackresolver.go` — re-add the field and wire the hook.
4. **Dirty-file tracking.** `Project.dirtyFilePath` and `dirtyFiles`.
5. **API dispatch.** `api/session.go`, `proto.go`, `callbackfs.go` — re-apply our
   method dispatch + `session_ls.go` exposures onto upstream's expanded generated
   dispatch.

## Not started

- **API JS layer (~18 files)** under `packages/typescript/src/api/**` and `src/ast/**`.
  Upstream rebuilt the TS API as a code generator (`proto.generated.ts`,
  `ast.generated.ts`, `node.generated.ts`, `encoder.generated.ts`) around a
  `Project`/`Checker`/`LanguageService`/`Emitter` object model. Our additions
  (WasmChannel, wasi.ts, sync client, sourceFileCache, node.infrastructure, wtf8,
  ast/children, ast/comments) are hand-edits on the pre-generator shape and must be
  re-expressed as generator inputs + object-model methods. Redundant now (drop):
  `getSymbolsInScope`, `getFullyQualifiedName`, find-references, completions,
  `Program.getTypeChecker` (-> `Project.checker`).
- **Wasm reactor rebuild** (`tsc/cmd/tsgo-wasm` -> typescript.wasm).
- **Verification**: the tsgo-wasm end-to-end scripts + differential harness.

## Assessment

The mechanical + straightforward-reconciliation work is done (9/11 Go packages). The
remainder is deliberate feature re-engineering against a rebuilt upstream, not porting.
Given both the in-process layer and the API surface must be re-expressed against the new
architecture regardless, doing it as targeted upstream PRs (see the decision report) is
the same work with a durable payoff.
