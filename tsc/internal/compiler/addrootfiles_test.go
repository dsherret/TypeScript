package compiler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/locale"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

// TestAddRootFilesMatchesRebuild is the proof that adding root files to a program
// gives the program a build from scratch would have given. Each case builds one both
// ways and compares everything the program can be asked: which files it holds and in
// what order, why each is in it, what every import resolved to, and every diagnostic.
//
// A case that says addable false is one the addition must refuse rather than get
// wrong, and the comparison is not made — refusing is the whole answer.
func TestAddRootFilesMatchesRebuild(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name         string
		files        map[string]string
		options      *core.CompilerOptions
		initialRoots []string
		addedRoots   []string
		addable      bool
	}{
		{
			name: "a file that imports nothing",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a file that imports one already in the program",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `import { a } from "./a"; export const b = a + 1;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a file that imports one that is not a root",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/dep.ts": `export const dep = 3;`,
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "several roots at once, importing each other",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `import { c } from "./c"; export const b = c;`,
				"/p/c.ts": `import { a } from "./a"; export const c = a;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts", "/p/c.ts"},
			addable:      true,
		},
		{
			name: "a root that is already in the program as an import",
			files: map[string]string{
				"/p/a.ts":   `import { dep } from "./dep"; export const a = dep;`,
				"/p/dep.ts": `export const dep = 3;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/dep.ts"},
			addable:      true,
		},
		{
			name: "a triple slash reference to a file already in the program",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference path=\"./a.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a triple slash reference to a file that is not",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/ref.ts": `declare const refd: number;`,
				"/p/b.ts":   "/// <reference path=\"./ref.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a lib reference the program does not already have",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference lib=\"es2015.symbol\" />\nexport const b = 2;",
			},
			options:      &core.CompilerOptions{Target: core.ScriptTargetES5, Lib: []string{"lib.es5.d.ts"}},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a declaration file beside a source file already in the program",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/a.d.ts": `export declare const a: number;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/a.d.ts"},
			addable:      true,
		},
		{
			name: "a root that does not exist",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/missing.ts"},
			addable:      true,
		},
		{
			name: "a root with an import that resolves to nothing",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `import { nope } from "./nope"; export const b = nope;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root from node_modules",
			files: map[string]string{
				"/p/a.ts":                                 `export const a = 1;`,
				"/p/node_modules/pkg/package.json":        `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":          `export declare const p: number;`,
				"/p/node_modules/other/package.json":      `{"name":"other","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/other/index.d.ts":        `export declare const o: number;`,
				"/p/node_modules/other/node_modules/x.ts": `export const x = 1;`,
				"/p/b.ts": `import { p } from "pkg"; export const b = p;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root whose name differs only in casing from one in the program",
			files: map[string]string{
				"/p/a.ts":    `export const a = 1;`,
				"/p/File.ts": `export const f = 1;`,
				"/p/file.ts": `export const g = 1;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/File.ts"},
			addedRoots:   []string{"/p/file.ts"},
			addable:      false,
		},
		{
			name: "a root outside rootDir",
			files: map[string]string{
				"/p/src/a.ts": `export const a = 1;`,
				"/p/out.ts":   `export const o = 1;`,
			},
			options:      &core.CompilerOptions{RootDir: "/p/src", ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/src/a.ts"},
			addedRoots:   []string{"/p/out.ts"},
			addable:      true,
		},
		{
			name: "a javascript root with allowJs off",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.js": `export const b = 2;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.js"},
			addable:      true,
		},
		{
			name: "a root that would be overwritten by another root's output",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/a.js": `export const a2 = 1;`,
			},
			options:      &core.CompilerOptions{AllowJs: core.TSTrue, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/a.js"},
			addable:      true,
		},
		{
			name: "a root with an automatic type directive already in the program",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that uses jsx",
			files: map[string]string{
				"/p/a.ts":                                   `export const a = 1;`,
				"/p/b.tsx":                                  `export const b = <div />;`,
				"/p/node_modules/react/package.json":        `{"name":"react","version":"18.0.0","types":"index.d.ts"}`,
				"/p/node_modules/react/index.d.ts":          `export declare const react: number;`,
				"/p/node_modules/react/jsx-runtime.d.ts":    `export declare const jsx: any;`,
				"/p/node_modules/react/jsx-runtime/index.d": ``,
			},
			options:      &core.CompilerOptions{Jsx: core.JsxEmitReactJSX, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.tsx"},
			addable:      false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			options := testCase.options
			if options == nil {
				options = &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
			}
			allRoots := slices.Concat(testCase.initialRoots, testCase.addedRoots)

			base := newTestProgram(testCase.files, options, testCase.initialRoots)
			// the base program is only comparable once everything lazy about it has
			// been asked for, which is also what a program in use has done
			base.verifyCompilerOptions()
			base.CommonSourceDirectory()

			added, _, ok := base.UpdateRootFiles(
				newTestConfig(options, allRoots),
				nil,
				base.Host(),
				nil,
			)
			assert.Equal(t, ok, testCase.addable, "UpdateRootFiles")
			if !ok {
				return
			}
			rebuilt := newTestProgram(testCase.files, options, allRoots)
			assertProgramsEquivalent(t, added, rebuilt)
		})
	}
}

// TestAddRootFilesReplacesChangedFiles covers the other half of what the ts-morph
// loop asks for: a root arriving while a file already in the program is being edited.
func TestAddRootFilesReplacesChangedFiles(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	base := newTestProgram(files, options, []string{"/p/a.ts"})
	base.verifyCompilerOptions()
	base.CommonSourceDirectory()

	edited := maps.Clone(files)
	edited["/p/a.ts"] = "export const a = 1;\n\nclass C {\n}\n"
	host := newTestHost(edited)

	added, _, ok := base.UpdateRootFiles(
		newTestConfig(options, []string{"/p/a.ts", "/p/b.ts"}),
		[]tspath.Path{"/p/a.ts"},
		host,
		nil,
	)
	assert.Assert(t, ok)
	assert.Equal(t, added.GetSourceFileByPath("/p/a.ts").Text(), edited["/p/a.ts"])

	rebuilt := newTestProgram(edited, options, []string{"/p/a.ts", "/p/b.ts"})
	assertProgramsEquivalent(t, added, rebuilt)
}

// TestAddRootFilesRefusesChangedImports covers the case the edit path also refuses:
// text that changed what a file imports cannot be swapped in, because everything it
// reaches has to be worked out again.
func TestAddRootFilesRefusesChangedImports(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts":   `export const a = 1;`,
		"/p/b.ts":   `export const b = 2;`,
		"/p/dep.ts": `export const dep = 3;`,
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	base := newTestProgram(files, options, []string{"/p/a.ts"})

	edited := maps.Clone(files)
	edited["/p/a.ts"] = `import { dep } from "./dep"; export const a = dep;`

	_, _, ok := base.UpdateRootFiles(
		newTestConfig(options, []string{"/p/a.ts", "/p/b.ts"}),
		[]tspath.Path{"/p/a.ts"},
		newTestHost(edited),
		nil,
	)
	assert.Assert(t, !ok)
}

// TestAddRootFilesRefusesADifferentConfig covers everything about a config that is
// not a change to its root file list.
func TestAddRootFilesRefusesADifferentConfig(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	base := newTestProgram(files, options, []string{"/p/a.ts"})

	refuses := map[string]*tsoptions.ParsedCommandLine{
		"an option that differs":  newTestConfig(&core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", Strict: core.TSTrue}, []string{"/p/a.ts", "/p/b.ts"}),
		"a root inserted first":   newTestConfig(options, []string{"/p/b.ts", "/p/a.ts"}),
		"nothing changed":         newTestConfig(options, []string{"/p/a.ts"}),
		"every root removed":      newTestConfig(options, []string{}),
		"the same roots reprefix": newTestConfig(options, []string{"/p/A.ts", "/p/a.ts", "/p/b.ts"}),
	}
	for name, config := range refuses {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, ok := base.UpdateRootFiles(config, nil, base.Host(), nil)
			assert.Assert(t, !ok)
		})
	}
}

func newTestHost(files map[string]string) CompilerHost {
	fs := vfstest.FromMap[any](nil, true /*useCaseSensitiveFileNames*/)
	for fileName, contents := range files {
		_ = fs.WriteFile(fileName, contents)
	}
	return NewCompilerHost("/p", bundled.WrapFS(fs), bundled.LibPath(), nil, nil)
}

func newTestConfig(options *core.CompilerOptions, roots []string) *tsoptions.ParsedCommandLine {
	return tsoptions.NewParsedCommandLine(options, roots, tspath.ComparePathsOptions{
		UseCaseSensitiveFileNames: true,
		CurrentDirectory:          "/p",
	})
}

func newTestProgram(files map[string]string, options *core.CompilerOptions, roots []string) *Program {
	return NewProgram(ProgramOptions{
		Config: newTestConfig(options, roots),
		Host:   newTestHost(files),
	})
}

func assertProgramsEquivalent(t *testing.T, added *Program, rebuilt *Program) {
	t.Helper()
	// every file, in order, with every reason it is in the program and everything
	// the program has worked out about where it came from
	assert.Equal(t, explainFiles(added), explainFiles(rebuilt), "explained files")
	// explained files renders a reason but not the data behind it, and a root file's
	// data is the name the config's file list gave it, which is what a diagnostic
	// about it points at
	assert.Equal(t, describeIncludeReasons(added), describeIncludeReasons(rebuilt), "include reasons")
	assert.Equal(t, describeResolutions(added), describeResolutions(rebuilt), "resolutions")
	assert.Equal(t, describeMetadata(added), describeMetadata(rebuilt), "file metadata")
	assert.DeepEqual(t, sortedFileNames(added.missingFiles), sortedFileNames(rebuilt.missingFiles))
	assert.DeepEqual(t, sortedPaths(added.libFiles), sortedPaths(rebuilt.libFiles))
	assert.DeepEqual(t, sortedPaths(added.filesByPath), sortedPaths(rebuilt.filesByPath))
	assert.DeepEqual(t, sortedPaths(added.jsxRuntimeImportSpecifiers), sortedPaths(rebuilt.jsxRuntimeImportSpecifiers))
	assert.DeepEqual(t, sortedPaths(added.importHelpersImportSpecifiers), sortedPaths(rebuilt.importHelpersImportSpecifiers))
	assert.DeepEqual(t, sortedPaths(added.redirectTargetsMap), sortedPaths(rebuilt.redirectTargetsMap))
	assert.DeepEqual(t, sortedPaths(added.redirectFilesByPath), sortedPaths(rebuilt.redirectFilesByPath))
	assert.DeepEqual(t, sortedSet(&added.sourceFilesFoundSearchingNodeModules), sortedSet(&rebuilt.sourceFilesFoundSearchingNodeModules))
	assert.DeepEqual(t, added.filesByLowerCasePath, rebuilt.filesByLowerCasePath)
	assert.Equal(t, added.libFileCount, rebuilt.libFileCount)
	assert.Equal(t, added.rootFilesEnd, rebuilt.rootFilesEnd)
	assert.Equal(t, added.CommonSourceDirectory(), rebuilt.CommonSourceDirectory())
	assert.Equal(t, diagnosticsText(added.GetProgramDiagnostics()), diagnosticsText(rebuilt.GetProgramDiagnostics()))
	assert.DeepEqual(t, sortedSet(&added.hasEmitBlockingDiagnostics), sortedSet(&rebuilt.hasEmitBlockingDiagnostics))
	assert.Equal(t, semanticDiagnosticsText(t, added), semanticDiagnosticsText(t, rebuilt), "semantic diagnostics")
	// every diagnostic the parse produced, including one about a file the program no
	// longer holds, which nothing above would go looking for
	assert.Equal(t, diagnosticsText(added.includeProcessor.getDiagnostics(added).GetDiagnostics()), diagnosticsText(rebuilt.includeProcessor.getDiagnostics(rebuilt).GetDiagnostics()), "include processor diagnostics")
}

func explainFiles(p *Program) string {
	var b strings.Builder
	p.ExplainFiles(&b, locale.Default)
	return b.String()
}

func describeIncludeReasons(p *Program) string {
	var lines []string
	for path, reasons := range p.includeProcessor.fileIncludeReasons {
		for index, reason := range reasons {
			lines = append(lines, fmt.Sprintf("%s [%d] kind=%v data=%s", path, index, reason.kind, describeIncludeReasonData(reason.data)))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// describeIncludeReasonData renders what a reason carries without its identity, since
// a synthesized import is a node each build makes for itself.
func describeIncludeReasonData(data any) string {
	referenced, ok := data.(*referencedFileData)
	if !ok {
		return fmt.Sprintf("%#v", data)
	}
	synthetic := "none"
	if referenced.synthetic != nil {
		synthetic = fmt.Sprintf("%v %q", referenced.synthetic.Kind, referenced.synthetic.Text())
	}
	return fmt.Sprintf("{file:%q index:%d synthetic:%s}", referenced.file, referenced.index, synthetic)
}

func describeResolutions(p *Program) string {
	var lines []string
	for path, resolutions := range p.resolvedModules {
		for key, resolution := range resolutions {
			lines = append(lines, fmt.Sprintf("%s %s(%d) -> %q %v", path, key.Name, key.Mode, resolution.ResolvedFileName, resolution.IsResolved()))
		}
	}
	for path, resolutions := range p.typeResolutionsInFile {
		for key, resolution := range resolutions {
			lines = append(lines, fmt.Sprintf("%s type %s(%d) -> %q", path, key.Name, key.Mode, resolution.ResolvedFileName))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func describeMetadata(p *Program) string {
	var lines []string
	for path, metadata := range p.sourceFileMetaDatas {
		lines = append(lines, fmt.Sprintf("%s %+v", path, metadata))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func semanticDiagnosticsText(t *testing.T, p *Program) string {
	t.Helper()
	var lines []string
	for _, file := range p.GetSourceFiles() {
		for _, diagnostic := range p.GetSemanticDiagnostics(context.Background(), file) {
			lines = append(lines, fmt.Sprintf("%s(%d): %s", file.FileName(), diagnostic.Pos(), diagnostic.Localize(locale.Default)))
		}
		for _, diagnostic := range p.GetIncludeProcessorDiagnostics(file) {
			lines = append(lines, fmt.Sprintf("%s(%d) include: %s", file.FileName(), diagnostic.Pos(), diagnostic.Localize(locale.Default)))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func diagnosticsText(diagnostics []*ast.Diagnostic) string {
	lines := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		var fileName string
		if diagnostic.File() != nil {
			fileName = diagnostic.File().FileName()
		}
		lines = append(lines, fmt.Sprintf("%s(%d): %s", fileName, diagnostic.Pos(), diagnostic.Localize(locale.Default)))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func sortedFileNames(names []string) []string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return sorted
}

func sortedPaths[V any](m map[tspath.Path]V) []string {
	names := make([]string, 0, len(m))
	for path := range m {
		names = append(names, string(path))
	}
	slices.Sort(names)
	return names
}

func sortedSet(s *collections.Set[tspath.Path]) []string {
	names := make([]string, 0, s.Len())
	for path := range s.Keys() {
		names = append(names, string(path))
	}
	slices.Sort(names)
	return names
}
