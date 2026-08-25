package compiler

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestAddRootFilesProbe is an adversarial second table for AddRootFiles: cases that
// try to make the incrementally extended program differ from one built from scratch.
func TestAddRootFilesProbe(t *testing.T) {
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
			// the added root reaches a file an automatic type directive brought in,
			// which a rebuild would place before the added root's own file
			name: "a root that references a file an automatic type directive brought in",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference path=\"./node_modules/@types/thing/index.d.ts\" />\nexport const b = 2;",
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root that type references a directive already in the program",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference types=\"thing\" />\nexport const b = 2;",
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root that references a file already missing from the program",
			files: map[string]string{
				"/p/a.ts": "/// <reference path=\"./nope.ts\" />\nexport const a = 1;",
				"/p/b.ts": "/// <reference path=\"./nope.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that is already in the program from node_modules",
			files: map[string]string{
				"/p/a.ts":                        `import { p } from "pkg"; export const a = p;`,
				"/p/node_modules/pkg/index.d.ts": `export declare const p: number;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/node_modules/pkg/index.d.ts"},
			addable:      false,
		},
		{
			name: "a root that type references a directive the program does not have",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference types=\"thing\" />\nexport const b = 2;",
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that brings in a second copy of a package",
			files: map[string]string{
				"/p/a.ts":                                           `import { p } from "pkg"; export const a = p;`,
				"/p/node_modules/pkg/package.json":                  `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":                    `export declare const p: number;`,
				"/p/node_modules/dep/package.json":                  `{"name":"dep","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/dep/index.d.ts":                    `import { p } from "pkg"; export declare const d: typeof p;`,
				"/p/node_modules/dep/node_modules/pkg/package.json": `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/dep/node_modules/pkg/index.d.ts":   `export declare const p: number;`,
				"/p/b.ts": `import { d } from "dep"; export const b = d;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			// the common case the depth rule must not refuse: an added root importing
			// a package the program already has, found by searching node_modules
			name: "a root that imports a package already in the program",
			files: map[string]string{
				"/p/a.ts":                        `import { p } from "pkg"; export const a = p;`,
				"/p/node_modules/pkg/index.d.ts": `export declare const p: number;`,
				"/p/b.ts":                        `import { p } from "pkg"; export const b = p;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that reaches a node_modules file in the program without searching",
			files: map[string]string{
				"/p/a.ts":                        `import { p } from "pkg"; export const a = p;`,
				"/p/node_modules/pkg/index.d.ts": `export declare const p: number;`,
				"/p/b.ts":                        "/// <reference path=\"./node_modules/pkg/index.d.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root that lib references one the program already has",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference lib=\"es2015.symbol\" />\nexport const b = 2;",
			},
			options:      &core.CompilerOptions{Target: core.ScriptTargetES5, Lib: []string{"lib.es5.d.ts", "lib.es2015.symbol.d.ts"}},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root that imports json the program already has",
			files: map[string]string{
				"/p/a.ts":   `import d from "./d.json"; export const a = d;`,
				"/p/d.json": `{"x":1}`,
				"/p/b.ts":   `import d from "./d.json"; export const b = d;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", ResolveJsonModule: core.TSTrue, Module: core.ModuleKindCommonJS},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that uses jsx when the runtime is already in the program",
			files: map[string]string{
				"/p/a.tsx":                             `export const a = <div />;`,
				"/p/b.tsx":                             `export const b = <span />;`,
				"/p/node_modules/react/jsx-runtime.ts": `export const jsx: any = 1; export const jsxs: any = 1; export const Fragment: any = 1;`,
			},
			options:      &core.CompilerOptions{Jsx: core.JsxEmitReactJSX, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.tsx"},
			addedRoots:   []string{"/p/b.tsx"},
			addable:      true,
		},
		{
			name: "a root reached by an existing file that is added after it",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/dep.ts": `export const dep = 3;`,
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts", "/p/dep.ts"},
			addable:      true,
		},
		{
			// verifyCompilerOptions runs again on the program that was added to, and
			// what it puts in the include processor must not be there twice
			name: "a root added to a composite project with a file outside the file list",
			files: map[string]string{
				"/p/a.ts":   `import { dep } from "./dep"; export const a = dep;`,
				"/p/dep.ts": `export const dep = 3;`,
				"/p/b.ts":   `export const b = 2;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", Composite: core.TSTrue, OutDir: "/p/out", RootDir: "/p"},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that imports a directory index",
			files: map[string]string{
				"/p/a.ts":         `export const a = 1;`,
				"/p/dir/index.ts": `export const d = 1;`,
				"/p/b.ts":         `import { d } from "./dir"; export const b = d;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that imports through paths",
			files: map[string]string{
				"/p/a.ts":     `export const a = 1;`,
				"/p/src/m.ts": `export const m = 1;`,
				"/p/b.ts":     `import { m } from "@app/m"; export const b = m;`,
			},
			options: &core.CompilerOptions{
				ConfigFilePath: "/p/tsconfig.json",
				BaseUrl:        "/p",
				Paths:          collectionsPaths,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that imports json",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/d.json": `{"x":1}`,
				"/p/b.ts":   `import d from "./d.json"; export const b = d;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", ResolveJsonModule: core.TSTrue, Module: core.ModuleKindCommonJS},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root that reaches a file differing only in casing from one in the program",
			files: map[string]string{
				"/p/a.ts":    `import { f } from "./File"; export const a = f;`,
				"/p/File.ts": `export const f = 1;`,
				"/p/file.ts": `export const g = 1;`,
				"/p/b.ts":    `import { g } from "./file"; export const b = g;`,
			},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      false,
		},
		{
			name: "a root that needs import helpers",
			files: map[string]string{
				"/p/a.ts":                          `export const a = 1;`,
				"/p/b.ts":                          `export * from "./c";`,
				"/p/c.ts":                          `export const c = 1;`,
				"/p/node_modules/tslib/index.d.ts": `export declare function __exportStar(m: any, e: any): void;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", ImportHelpers: core.TSTrue, Module: core.ModuleKindCommonJS},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.ts"},
			addable:      true,
		},
		{
			name: "a root outside rootDir added to a program that already has one outside",
			files: map[string]string{
				"/p/src/a.ts": `export const a = 1;`,
				"/p/one.ts":   `export const o = 1;`,
				"/p/two.ts":   `export const t = 1;`,
			},
			options:      &core.CompilerOptions{RootDir: "/p/src", OutDir: "/p/out", ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/src/a.ts", "/p/one.ts"},
			addedRoots:   []string{"/p/two.ts"},
			addable:      true,
		},
		{
			name: "a root that changes the inferred common source directory",
			files: map[string]string{
				"/p/src/a.ts":   `export const a = 1;`,
				"/p/other/b.ts": `export const b = 1;`,
			},
			options:      &core.CompilerOptions{OutDir: "/out"},
			initialRoots: []string{"/p/src/a.ts"},
			addedRoots:   []string{"/p/other/b.ts"},
			addable:      true,
		},
		{
			name: "a javascript root with allowJs and maxNodeModuleJsDepth",
			files: map[string]string{
				"/p/a.ts":                      `export const a = 1;`,
				"/p/b.js":                      `const x = require("pkg"); module.exports = x;`,
				"/p/node_modules/pkg/index.js": `module.exports = 1;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", AllowJs: core.TSTrue, Module: core.ModuleKindCommonJS},
			initialRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/b.js"},
			addable:      true,
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
			base.verifyCompilerOptions()
			base.CommonSourceDirectory()
			before := programFingerprint(base)

			added, _, ok := base.UpdateRootFiles(
				newTestConfig(options, allRoots),
				nil,
				base.Host(),
				nil,
			)
			assert.Equal(t, ok, testCase.addable, "UpdateRootFiles")
			// whatever happened, the program that was added to must still answer
			// exactly as it did before
			assert.Equal(t, programFingerprint(base), before, "base program mutated")
			if !ok {
				return
			}
			assertChangedPathsAreTheDiff(t, base, added)
			rebuilt := newTestProgram(testCase.files, options, allRoots)
			assertProgramsEquivalent(t, added, rebuilt)
			assert.Equal(t, programFingerprint(base), before, "base program mutated after comparison")
		})
	}
}

var collectionsPaths = func() *collections.OrderedMap[string, []string] {
	m := &collections.OrderedMap[string, []string]{}
	m.Set("@app/*", []string{"./src/*"})
	return m
}()

// programFingerprint is everything about a program that another program built from
// it must not have changed.
func programFingerprint(p *Program) string {
	var b strings.Builder
	b.WriteString(explainFiles(p))
	b.WriteString("\n--- files ---\n")
	for _, file := range p.files {
		fmt.Fprintf(&b, "%s\n", file.FileName())
	}
	b.WriteString("\n--- resolutions ---\n")
	b.WriteString(describeResolutions(p))
	b.WriteString("\n--- metadata ---\n")
	b.WriteString(describeMetadata(p))
	b.WriteString("\n--- missing ---\n")
	b.WriteString(strings.Join(p.missingFiles, "\n"))
	b.WriteString("\n--- node modules ---\n")
	b.WriteString(strings.Join(sortedSet(&p.sourceFilesFoundSearchingNodeModules), "\n"))
	b.WriteString("\n--- lower case ---\n")
	lower := make([]string, 0, len(p.filesByLowerCasePath))
	for name, path := range p.filesByLowerCasePath {
		lower = append(lower, name+" -> "+string(path))
	}
	slices.Sort(lower)
	b.WriteString(strings.Join(lower, "\n"))
	b.WriteString("\n--- duplicates ---\n")
	dups := make([]string, 0, len(p.duplicateSourceFiles))
	for _, dup := range p.duplicateSourceFiles {
		dups = append(dups, dup.ParseOptions.FileName)
	}
	slices.Sort(dups)
	b.WriteString(strings.Join(dups, "\n"))
	fmt.Fprintf(&b, "\n--- counts --- %d %d %d\n", p.libFileCount, p.rootFilesEnd, len(p.files))
	b.WriteString("\n--- diagnostics ---\n")
	b.WriteString(diagnosticsText(p.GetProgramDiagnostics()))
	b.WriteString("\n--- include diagnostics ---\n")
	var lines []string
	for _, file := range p.GetSourceFiles() {
		for _, d := range p.GetIncludeProcessorDiagnostics(file) {
			lines = append(lines, fmt.Sprintf("%s(%d): %s", file.FileName(), d.Pos(), d.Localize(locale.Default)))
		}
	}
	slices.Sort(lines)
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n--- common source directory ---\n")
	b.WriteString(p.CommonSourceDirectory())
	return b.String()
}

// TestAddRootFilesChained adds roots to a program that was itself made by adding
// roots, which is the shape ts-morph's codegen loop makes over and over.
func TestAddRootFilesChained(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
		"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
	}
	options := &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json", RootDir: "/p", OutDir: "/out"}
	roots := []string{"/p/a.ts"}
	program := newTestProgram(files, options, roots)
	program.verifyCompilerOptions()
	program.CommonSourceDirectory()

	for _, added := range [][]string{
		{"/p/b.ts"},
		{"/p/c.ts", "/p/d.ts"},
		{"/p/e.ts"},
	} {
		for _, name := range added {
			files[name] = fmt.Sprintf("import { a } from %q; export const x = a;", "./a")
		}
		roots = append(roots, added...)
		next, _, ok := program.UpdateRootFiles(newTestConfig(options, roots), nil, newTestHost(files), nil)
		assert.Assert(t, ok, "UpdateRootFiles %v", added)
		assertChangedPathsAreTheDiff(t, program, next)
		assertProgramsEquivalent(t, next, newTestProgram(files, options, roots))
		program = next
	}
}

// TestAddRootFilesReplacesSeveralChangedFiles covers more than one file changing in
// the batch a root arrives in, which the single file edit path used to refuse.
func TestAddRootFilesReplacesSeveralChangedFiles(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
		"/p/c.ts": `export const c = 3;`,
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	base := newTestProgram(files, options, []string{"/p/a.ts", "/p/b.ts"})
	base.verifyCompilerOptions()
	base.CommonSourceDirectory()
	before := programFingerprint(base)

	edited := maps.Clone(files)
	edited["/p/a.ts"] = "export const a = 1;\nclass A {}\n"
	edited["/p/b.ts"] = "export const b = 2;\nclass B {}\n"

	added, _, ok := base.UpdateRootFiles(
		newTestConfig(options, []string{"/p/a.ts", "/p/b.ts", "/p/c.ts"}),
		[]tspath.Path{"/p/a.ts", "/p/b.ts"},
		newTestHost(edited),
		nil,
	)
	assert.Assert(t, ok)
	assert.Equal(t, programFingerprint(base), before, "base program mutated")
	assert.Equal(t, added.GetSourceFileByPath("/p/a.ts").Text(), edited["/p/a.ts"])
	assert.Equal(t, added.GetSourceFileByPath("/p/b.ts").Text(), edited["/p/b.ts"])
	assertChangedPathsAreTheDiff(t, base, added)
	assertProgramsEquivalent(t, added, newTestProgram(edited, options, []string{"/p/a.ts", "/p/b.ts", "/p/c.ts"}))
}

// assertChangedPathsAreTheDiff checks what a program built from another says it
// changed and took away against comparing the two file maps outright, which is what
// the API session reports to the client when a program cannot say. The two have to
// name the same files.
func assertChangedPathsAreTheDiff(t *testing.T, base *Program, derived *Program) {
	t.Helper()
	reportedChanged, reportedRemoved, ok := derived.FilesChangedFrom(base)
	assert.Assert(t, ok, "the derived program could not say what it changed")
	var changed, removed []string
	for path, file := range base.filesByPath {
		now, held := derived.filesByPath[path]
		if !held {
			removed = append(removed, string(path))
		} else if now != file {
			changed = append(changed, string(path))
		}
	}
	assert.Equal(t, joinSortedPaths(reportedChanged), joinSorted(changed), "changed paths")
	assert.Equal(t, joinSortedPaths(reportedRemoved), joinSorted(removed), "removed paths")
}

func joinSortedPaths(paths []tspath.Path) string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, string(path))
	}
	return joinSorted(names)
}

func joinSorted(names []string) string {
	slices.Sort(names)
	return strings.Join(names, ", ")
}
