# Porting the ts-morph fork onto microsoft/TypeScript main

Branch `migrate-tsmain`, based on `microsoft/TypeScript` main (`e533f4c083`).

## Status: builds, runs, and every deferred feature and optimization is in

All three layers build on TypeScript main and the reactor runs in-process, and the last
deferred optimization — incremental roots — is now ported and wired (see below):

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
- **parseSourceFile** (parse-without-snapshot perf) — Go handler + JS method, with the
  offer cache-reuse now wired: `ParseSourceFile` retains the parsed tree in the parse
  cache and `updateSnapshot` releases the offers once the next snapshot's programs have
  had their chance to take them over, so an edit followed by a semantic question reuses
  the tree instead of parsing it twice (see `TestParseSourceFileOfferLifetime`).

JS note: `apiRequest`/`apiRequestBinary` are strongly typed via the generated
`APIMethodInfo` registry. Methods not (yet) in that registry are called through a cast.
Wiring them into `tools/gen-proto` would remove the casts.

## The last optimization: incremental roots — ported and wired

**Done.** A program now adds or removes root files (and updates a single changed file)
by deriving the next program from the previous one rather than rebuilding it. The
~755-line compiler port (`processRootFileChanges` / `diffRootFiles` / `canUpdateRootFiles`
/ `replacementsFor` on the rebuilt `processedFiles`, plus `rootFilesEnd`, `libFileCount`,
`filesByLowerCasePath`, and the `removedRoots` set) was committed earlier; this pass wired
it into the project layer and fixed the reference accounting so it is actually used:

- **Wired into `Project.CreateProgram`** — the derive-from-previous branch
  (`updateRootFilesInProgram`) now sits between the single-file update and the full
  rebuild. It fires on API/config root changes; content-only and unknown changes fall
  through to the paths they always took.
- **API root files reach the program** — `CreateProgram` builds from
  `effectiveCommandLine()` (config + typings + the roots an API client named), not the
  config alone.
- **Change tracking unified** — the vestigial singular `dirtyFilePath` (never set on
  this branch) was dropped for the fork's `dirtyFiles`/`deletedFiles`/`dirtyFilesKnown`,
  which `markFilesChanged` populates and the single-file branch now reads.
- **Parse-cache accounting made sound** — root reasons carry the file *name*, not a
  config index (stable across incremental add/remove; see `asRootFileName`); the
  incremental walk records every freshly-acquired file in `loader.acquired`; and the
  derived program's ref/deref loops skip stub/supplemental files and route content-mapped
  files to their own cache, exactly the way the disposing snapshot releases them. Without
  these the cache leaked (a new root counted twice) or panicked ("cache entry not found").
- **`parseSourceFile` offers are released** — `updateSnapshot` now calls
  `releaseOfferedFiles` after building the snapshot's programs, so an offered tree the
  next program took over is held once, not twice.

Verified by the fork's equivalence tests (incremental program == full rebuild) at the
compiler level and by the project-level `TestAPIRoots*` / `TestAddRootsProjectLevel*` /
`TestParseSourceFileOfferLifetime` reference-count tests, plus the 5 Wasm smoke tests.

### Known unrelated failure

`TestProcessChanges/extensionless_disk_file_preserves_unknown_script_kind` fails, and did
before this work: the migration deliberately made `diskFile.Kind()` default an
extensionless file to `ScriptKindTS` (upstream left it `ScriptKindUnknown`). It is a
standing ts-morph choice in the overlay FS, not part of the incremental-roots path.
