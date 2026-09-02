package api

import (
	"context"
	"sync"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/module"
	"github.com/microsoft/TypeScript/tsc/internal/project"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/projecttestutil"
	"gotest.tools/v3/assert"
)

// The host resolution hook lets a client resolve a module specifier the compiler
// would resolve differently, or reject. It is consulted in Resolver.ResolveModuleName
// before the compiler's own resolution, and the answer is believed.
//
// The three answers are: decline (resolve normally), resolve to a file, and
// rewrite (resolve this other specifier instead, using the compiler's rules).

// A Deno-style importer: the specifier carries the extension node omits.
const denoStyleMain = `import { Test } from "./Test.ts";
const test: Test = null as any;
`

// errorCodeImportPathCannotEndWithTsExtension is 5097, which is what makes the
// rewrite observable: the file behind "./Test.ts" is found either way, but the
// specifier as written is rejected unless allowImportingTsExtensions is on.
const errorCodeImportPathCannotEndWithTsExtension = 5097

func TestResolveModuleNameHookRewritesSpecifier(t *testing.T) {
	t.Parallel()
	codes := semanticDiagnosticCodesWithHook(t, denoStyleMain, func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool) {
		if len(moduleName) > 3 && moduleName[len(moduleName)-3:] == ".ts" {
			return &module.HostModuleResolution{ModuleName: moduleName[:len(moduleName)-3]}, true
		}
		return nil, false
	})
	assert.DeepEqual(t, codes, []int32{})
}

// Without the hook the same program reports 5097, which is what proves the test
// above is measuring the hook rather than the compiler being lenient.
func TestResolveModuleNameHookAbsentLeavesTheCompilerError(t *testing.T) {
	t.Parallel()
	codes := semanticDiagnosticCodesWithHook(t, denoStyleMain, nil)
	assert.DeepEqual(t, codes, []int32{errorCodeImportPathCannotEndWithTsExtension})
}

// Declining every question has to behave exactly as having no hook at all.
func TestResolveModuleNameHookDeclining(t *testing.T) {
	t.Parallel()
	codes := semanticDiagnosticCodesWithHook(t, denoStyleMain, func(string, string, core.ResolutionMode) (*module.HostModuleResolution, bool) {
		return nil, false
	})
	assert.DeepEqual(t, codes, []int32{errorCodeImportPathCannotEndWithTsExtension})
}

// A host may name the file outright, for a specifier the compiler could never
// resolve on its own.
func TestResolveModuleNameHookResolvesOutright(t *testing.T) {
	t.Parallel()
	const main = `import { Test } from "alias";
const test: Test = null as any;
`
	codes := semanticDiagnosticCodesWithHook(t, main, func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool) {
		if moduleName == "alias" {
			return &module.HostModuleResolution{
				Resolved: &module.ResolvedModule{
					ResolvedFileName: "/home/projects/p/Test.ts",
					Extension:        ".ts",
				},
			}, true
		}
		return nil, false
	})
	assert.DeepEqual(t, codes, []int32{})
}

// Answering with a nil resolution is "resolves to nothing, and do not let the
// compiler try" — distinct from declining, and it must not crash the checker.
func TestResolveModuleNameHookResolvesToNothing(t *testing.T) {
	t.Parallel()
	const main = `import { Test } from "./Test";
const test: Test = null as any;
`
	codes := semanticDiagnosticCodesWithHook(t, main, func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool) {
		if moduleName == "./Test" {
			return &module.HostModuleResolution{}, true
		}
		return nil, false
	})
	// 2307: cannot find module. The point is that it is reported rather than the
	// compiler resolving the file itself, and that a nil *ResolvedModule does not
	// reach the checker.
	assert.DeepEqual(t, codes, []int32{2307})
}

// The mode is per import rather than per file, so a host can answer differently
// for an ESM and a CommonJS importer.
func TestResolveModuleNameHookReceivesResolutionMode(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	var mu sync.Mutex
	seen := map[string]core.ResolutionMode{}

	files := map[string]any{
		"/home/projects/p/tsconfig.json": `{ "compilerOptions": { "module": "nodenext", "moduleResolution": "nodenext" } }`,
		"/home/projects/p/dep.mts":       "export const a = 1;",
		"/home/projects/p/dep2.cts":      "export const b = 1;",
		"/home/projects/p/esm.mts":       `import { a } from "./dep.mjs";`,
		"/home/projects/p/cjs.cts":       `import { b } from "./dep2.cjs";`,
	}
	options := defaultSessionOptions()
	options.ResolveModuleName = func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool) {
		mu.Lock()
		defer mu.Unlock()
		seen[moduleName] = resolutionMode
		return nil, false
	}

	projectSession, _ := projecttestutil.SetupWithOptions(files, options)
	defer projectSession.Close()
	session := NewSession(projectSession, nil)
	defer session.Close()

	for _, fileName := range []string{"/home/projects/p/esm.mts", "/home/projects/p/cjs.cts"} {
		requestSemanticDiagnostics(t, session, fileName)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, seen["./dep.mjs"], core.ModuleKindESNext)
	assert.Equal(t, seen["./dep2.cjs"], core.ModuleKindCommonJS)
}

// semanticDiagnosticCodesWithHook compiles a two-file project with the given hook
// and returns the codes of main.ts's semantic diagnostics. A nil hook installs none.
func semanticDiagnosticCodesWithHook(
	t *testing.T,
	mainContent string,
	hook func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool),
) []int32 {
	t.Helper()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const mainFileName = "/home/projects/p/main.ts"
	files := map[string]any{
		"/home/projects/p/tsconfig.json": `{ "compilerOptions": { "strict": true } }`,
		"/home/projects/p/Test.ts":       "export class Test {}",
		mainFileName:                     mainContent,
	}

	options := defaultSessionOptions()
	options.ResolveModuleName = hook

	projectSession, _ := projecttestutil.SetupWithOptions(files, options)
	defer projectSession.Close()
	session := NewSession(projectSession, nil)
	defer session.Close()

	return requestSemanticDiagnostics(t, session, mainFileName)
}

func requestSemanticDiagnostics(t *testing.T, session *Session, fileName string) []int32 {
	t.Helper()
	ctx := context.Background()

	snapshotResp, err := session.handleUpdateSnapshot(ctx, &UpdateSnapshotParams{
		OpenFiles: []DocumentIdentifier{{FileName: fileName}},
	})
	assert.NilError(t, err)

	proj, err := session.handleGetDefaultProjectForFile(ctx, &GetDefaultProjectForFileParams{
		Snapshot: snapshotResp.Snapshot,
		File:     DocumentIdentifier{FileName: fileName},
	})
	assert.NilError(t, err)
	assert.Assert(t, proj != nil, "file should resolve to a default project")

	diagnostics, err := session.handleGetSemanticDiagnostics(ctx, &GetDiagnosticsParams{
		Snapshot: snapshotResp.Snapshot,
		Project:  proj.Id,
		Files:    []DocumentIdentifier{{FileName: fileName}},
	})
	assert.NilError(t, err)

	codes := []int32{}
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	return codes
}

func defaultSessionOptions() *project.SessionOptions {
	return &project.SessionOptions{
		CurrentDirectory:       "/",
		DefaultLibraryPath:     bundled.LibPath(),
		TypingsLocation:        projecttestutil.TestTypingsLocation,
		PositionEncoding:       lsproto.PositionEncodingKindUTF8,
		WatchEnabled:           true,
		LoggingEnabled:         true,
		PushDiagnosticsEnabled: true,
	}
}
