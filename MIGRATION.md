# Porting the ts-morph fork onto microsoft/TypeScript main

Branch `migrate-tsmain`, based on `microsoft/TypeScript` main (`e533f4c083`).

## Status: builds, runs, and the deferred features are restored

All three layers build on TypeScript main and the reactor runs in-process:

    go build ./...                                 exit 0   (11/11 Go packages)
    GOOS=wasip1 GOARCH=wasm ... -buildmode=c-shared  exit 0   (cmd/tsgo-wasm reactor)
    tsc -b  (packages/typescript)                  exit 0   (JS API layer)

Five end-to-end smoke tests pass against the built reactor (`packages/typescript/smoke*.mjs`):
core parse+check, LS rename+format, checker getAmbientModules, batched
getExportedSymbolsOfFiles, and parse-without-snapshot.

## Restored features (were deferred to reach the first build)

- **LS edit exposures** — rename, formatDocument, formatDocumentRange, organizeImports,
  getDefinition, getImplementations, getCodeFixes, getCombinedCodeFix, getAmbientModules.
  Go: restored `session_ls.go`, adapted every call to upstream's rewritten `lsconv`
  converter API (package functions `FromLSPPositionForSourceFile`, `ToLSPRange`/
  `ToLSPPosition` 2-return, `toAPITextEdits` arity), re-wired constants/unmarshallers/
  dispatch. JS: added the `Project` methods + proto types.
- **Checker exposures** — getSymbolOfDeclaration, symbolToString, getAmbientModules.
- **getExportedSymbolsOfFiles** (batched-exports perf) — Go handler + JS method. Also
  fixed a real bug in `DocumentIdentifier.UnmarshalJSONFrom` that dropped `fileName` in
  the object case.
- **parseSourceFile** (parse-without-snapshot perf) — Go handler + JS method. The
  `SourceFileCache#offer` cache-reuse micro-optimization is deferred (its fork hunk
  rejected); the method parses and returns correctly without it.

JS note: `apiRequest`/`apiRequestBinary` are strongly typed via the generated
`APIMethodInfo` registry. Methods not (yet) in that registry are called through a cast.
Wiring them into `tools/gen-proto` would remove the casts.

## The one remaining optimization: incremental roots

Currently **stubbed to a correct full-rebuild fallback** (`Program.RootFileChangesFrom` /
`UpdateRootFiles` / `FilesChangedFrom` return "no incremental update", so callers rebuild
the program). Functionality is correct; the optimization is not yet ported.

Re-implementing it is a genuine ~755-line effort against a rewritten architecture:
`processRootFileChanges` / `diffRootFiles` / `canUpdateRootFiles` / `replacementsFor`
operate on `processedFiles`, which upstream rebuilt around content-mappers, a
project-reference file mapper, and an include-processor. Porting means adding the fork's
fields (`rootFilesEnd`, `libFileCount`, `filesByLowerCasePath`, a `removedRoots` set) to
the new `processedFiles` and re-expressing the incremental extend/remove logic on top of
the new file-loading flow. High-risk (program-state correctness), so it is left as a
deliberate follow-up rather than rushed.

## Also deferred (smaller)

- `getSourceFileIdentity`, project-root naming (`getProjectRootFiles`,
  `WithAdditionalRootFiles`) — needed only by the incremental-roots path.
- `SourceFileCache#offer` cache reuse for `parseSourceFile`.
