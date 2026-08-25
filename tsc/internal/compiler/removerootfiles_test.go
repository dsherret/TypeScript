package compiler

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestRemoveRootFilesMatchesRebuild is the proof that dropping root files from a
// program gives the program a build from scratch would have given. Each case builds one
// both ways and compares everything the program can be asked: which files it holds and
// in what order, why each is in it, what every import resolved to, and every diagnostic.
//
// A case that says removable false is one the removal must refuse rather than get
// wrong, and the comparison is not made — refusing is the whole answer. The removals
// that have to be refused are the point of the suite rather than an afterthought: a
// file leaving can move what other files resolve to, which is the one thing an addition
// appended to the end can never do.
func TestRemoveRootFilesMatchesRebuild(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name         string
		files        map[string]string
		options      *core.CompilerOptions
		initialRoots []string
		removedRoots []string
		addedRoots   []string
		removable    bool
	}{
		{
			name: "a root nothing else in the program refers to",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "the first root rather than the last",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/c.ts": `export const c = 3;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts", "/p/c.ts"},
			removedRoots: []string{"/p/a.ts"},
			removable:    true,
		},
		{
			name: "several roots at once",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/c.ts": `export const c = 3;`,
				"/p/d.ts": `export const d = 4;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts", "/p/c.ts", "/p/d.ts"},
			removedRoots: []string{"/p/a.ts", "/p/c.ts"},
			removable:    true,
		},
		{
			name: "a root another file imports",
			files: map[string]string{
				"/p/a.ts": `import { b } from "./b"; export const a = b;`,
				"/p/b.ts": `export const b = 2;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a root that imports one only it reaches",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
				"/p/dep.ts": `export const dep = 3;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a root that imports one an earlier root already brought in",
			files: map[string]string{
				"/p/dep.ts": `export const dep = 3;`,
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
			},
			initialRoots: []string{"/p/dep.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "a root that imports one a later root would have brought in",
			files: map[string]string{
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
				"/p/dep.ts": `export const dep = 3;`,
			},
			initialRoots: []string{"/p/b.ts", "/p/dep.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			// dep.ts sits where b.ts reached it, which is before c.ts; a rebuild
			// without b.ts would place it after c.ts instead
			name: "a root whose leaving would reorder the files that stay",
			files: map[string]string{
				"/p/b.ts":   `import { dep } from "./dep"; export const b = dep;`,
				"/p/c.ts":   `export const c = 3;`,
				"/p/dep.ts": `export const dep = 3;`,
			},
			initialRoots: []string{"/p/b.ts", "/p/c.ts", "/p/dep.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a declaration file beside the implementation another file imports",
			files: map[string]string{
				"/p/a.ts":   `export declare const a: number;`,
				"/p/a.d.ts": `export declare const a: string;`,
				"/p/b.ts":   `import { a } from "./a"; export const b = a;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/a.d.ts", "/p/b.ts"},
			removedRoots: []string{"/p/a.d.ts"},
			removable:    true,
		},
		{
			name: "the implementation beside the declaration file another file imports",
			files: map[string]string{
				"/p/a.ts":   `export declare const a: number;`,
				"/p/a.d.ts": `export declare const a: string;`,
				"/p/b.ts":   `import { a } from "./a"; export const b = a;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/a.d.ts", "/p/b.ts"},
			removedRoots: []string{"/p/a.ts"},
			removable:    false,
		},
		{
			name: "a root a package under node_modules would be found instead of",
			files: map[string]string{
				"/p/a.ts":                            `import { pkg } from "pkg"; export const a = pkg;`,
				"/p/node_modules/pkg/package.json":   `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":     `export declare const pkg: number;`,
				"/p/node_modules/pkg/other.d.ts":     `export declare const other: number;`,
				"/p/node_modules/pkg/notatypes.d.ts": `export declare const nope: number;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/node_modules/pkg/other.d.ts"},
			removedRoots: []string{"/p/node_modules/pkg/other.d.ts"},
			removable:    true,
		},
		{
			name: "the package entry point a root also names",
			files: map[string]string{
				"/p/a.ts":                          `import { pkg } from "pkg"; export const a = pkg;`,
				"/p/node_modules/pkg/package.json": `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":   `export declare const pkg: number;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/node_modules/pkg/index.d.ts"},
			removedRoots: []string{"/p/node_modules/pkg/index.d.ts"},
			removable:    false,
		},
		{
			// the whole hazard in one case: a.ts imports "pkg", which paths resolves to
			// the root that is going, and a rebuild would find the node_modules copy
			name: "a root a bare import resolves to instead of the node_modules copy",
			files: map[string]string{
				"/p/pkg.ts":                        `export const pkg = 1;`,
				"/p/a.ts":                          `import { pkg } from "pkg"; export const a = pkg;`,
				"/p/node_modules/pkg/package.json": `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":   `export declare const pkg: number;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", BaseUrl: "/p", Paths: removeRootsPkgPaths},
			initialRoots: []string{"/p/pkg.ts", "/p/a.ts"},
			removedRoots: []string{"/p/pkg.ts"},
			removable:    false,
		},
		{
			// the same shape with nothing importing it, which is safe and has to stay so
			name: "a root a bare import would resolve to, that nothing imports",
			files: map[string]string{
				"/p/pkg.ts":                        `export const pkg = 1;`,
				"/p/a.ts":                          `export const a = 1;`,
				"/p/node_modules/pkg/package.json": `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/pkg/index.d.ts":   `export declare const pkg: number;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", BaseUrl: "/p", Paths: removeRootsPkgPaths},
			initialRoots: []string{"/p/pkg.ts", "/p/a.ts"},
			removedRoots: []string{"/p/pkg.ts"},
			removable:    true,
		},
		{
			name: "a root that triple slash references a file only it reaches",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/ref.ts": `declare const refd: number;`,
				"/p/b.ts":   "/// <reference path=\"./ref.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a root that triple slash references a file that does not exist",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference path=\"./nope.ts\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a root that lib references a file the program would keep",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": "/// <reference lib=\"es2015.symbol\" />\nexport const b = 2;",
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			name: "a root with an import that resolves to nothing",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `import { nope } from "./nope"; export const b = nope;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "a root with an import of a package that is not installed",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `import { nope } from "nowhere"; export const b = nope;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "a root that is the only one left",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
			},
			initialRoots: []string{"/p/a.ts"},
			removedRoots: []string{"/p/a.ts"},
			removable:    false,
		},
		{
			name: "a root in a project with an automatic type directive",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "a root that reaches what an automatic type directive brought in",
			files: map[string]string{
				"/p/a.ts":                                   `export const a = 1;`,
				"/p/b.ts":                                   `import "thing"; export const b = 2;`,
				"/p/node_modules/thing/package.json":        `{"name":"thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/thing/index.d.ts":          `declare const thing: number; export {};`,
				"/p/node_modules/@types/thing/package.json": `{"name":"@types/thing","version":"1.0.0","types":"index.d.ts"}`,
				"/p/node_modules/@types/thing/index.d.ts":   `declare const thing: number;`,
			},
			options:      &core.CompilerOptions{Types: []string{"thing"}, ConfigFilePath: "/p/tsconfig.json"},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    false,
		},
		{
			// the index of files by lower case name holds the first path of each
			// casing, so the second one leaving would take the first one's entry
			name: "a root that differs from another in nothing but casing",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/B.ts": `export const bigB = 2;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts", "/p/B.ts"},
			removedRoots: []string{"/p/B.ts"},
			removable:    false,
		},
		{
			name: "a root under a rootDir with an outDir",
			files: map[string]string{
				"/p/src/a.ts": `export const a = 1;`,
				"/p/src/b.ts": `export const b = 2;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", RootDir: "/p/src", OutDir: "/out"},
			initialRoots: []string{"/p/src/a.ts", "/p/src/b.ts"},
			removedRoots: []string{"/p/src/b.ts"},
			removable:    true,
		},
		{
			// the common source directory is not carried over here, since the parse
			// said something about the file that is going; the derived program works
			// it out over its own files instead
			name: "a root outside the rootDir, which is what the diagnostic was about",
			files: map[string]string{
				"/p/src/a.ts":   `export const a = 1;`,
				"/p/other/b.ts": `export const b = 2;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", RootDir: "/p/src", OutDir: "/out"},
			initialRoots: []string{"/p/src/a.ts", "/p/other/b.ts"},
			removedRoots: []string{"/p/other/b.ts"},
			removable:    true,
		},
		{
			name: "a root that pins the inferred common source directory",
			files: map[string]string{
				"/p/src/a.ts":   `export const a = 1;`,
				"/p/other/b.ts": `export const b = 2;`,
			},
			options:      &core.CompilerOptions{OutDir: "/out"},
			initialRoots: []string{"/p/src/a.ts", "/p/other/b.ts"},
			removedRoots: []string{"/p/other/b.ts"},
			removable:    true,
		},
		{
			name: "a root with a semantic error of its own",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b: string = 2;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/b.ts"},
			removable:    true,
		},
		{
			name: "a root that declares a global another file uses",
			files: map[string]string{
				"/p/a.ts":      `declare global { const shared: number; } export {};`,
				"/p/uses.ts":   `export const uses = shared;`,
				"/p/global.ts": `declare const shared: number;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/uses.ts", "/p/global.ts"},
			removedRoots: []string{"/p/global.ts"},
			removable:    true,
		},
		{
			name: "a root removed while another arrives",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/c.ts": `export const c = 3;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/c.ts"},
			removable:    true,
		},
		{
			name: "a root removed while one that imports it arrives",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
				"/p/c.ts": `import { a } from "./a"; export const c = a;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/c.ts"},
			removable:    false,
		},
		{
			name: "a root removed and named again in the same breath",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.ts": `export const b = 2;`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.ts"},
			removedRoots: []string{"/p/a.ts"},
			addedRoots:   []string{"/p/a.ts"},
			removable:    false,
		},
		{
			name: "a javascript root with allowJs",
			files: map[string]string{
				"/p/a.ts": `export const a = 1;`,
				"/p/b.js": `module.exports = 1;`,
			},
			options:      &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json", AllowJs: core.TSTrue, Module: core.ModuleKindCommonJS},
			initialRoots: []string{"/p/a.ts", "/p/b.js"},
			removedRoots: []string{"/p/b.js"},
			removable:    true,
		},
		{
			name: "a json root",
			files: map[string]string{
				"/p/a.ts":   `export const a = 1;`,
				"/p/b.json": `{}`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.json"},
			removedRoots: []string{"/p/b.json"},
			removable:    true,
		},
		{
			// nothing parses it, so the program never held it and there is nothing
			// to say about what it would mean to take it away
			name: "a root with an unsupported extension, which is not in the program at all",
			files: map[string]string{
				"/p/a.ts":  `export const a = 1;`,
				"/p/b.txt": `not typescript`,
			},
			initialRoots: []string{"/p/a.ts", "/p/b.txt"},
			removedRoots: []string{"/p/b.txt"},
			removable:    false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			options := testCase.options
			if options == nil {
				options = &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
			}
			removed := make(map[string]struct{}, len(testCase.removedRoots))
			for _, name := range testCase.removedRoots {
				removed[name] = struct{}{}
			}
			remainingRoots := slices.DeleteFunc(slices.Clone(testCase.initialRoots), func(name string) bool {
				_, isRemoved := removed[name]
				return isRemoved
			})
			finalRoots := slices.Concat(remainingRoots, testCase.addedRoots)

			base := newTestProgram(testCase.files, options, testCase.initialRoots)
			// the base program is only comparable once everything lazy about it has
			// been asked for, which is also what a program in use has done
			base.verifyCompilerOptions()
			base.CommonSourceDirectory()
			before := programFingerprint(base)

			derived, _, ok := base.UpdateRootFiles(
				newTestConfig(options, finalRoots),
				nil,
				base.Host(),
				nil,
			)
			assert.Equal(t, ok, testCase.removable, "UpdateRootFiles")
			// whatever happened, the program that was derived from must still answer
			// exactly as it did before
			assert.Equal(t, programFingerprint(base), before, "base program mutated")
			if !ok {
				return
			}
			assertChangedPathsAreTheDiff(t, base, derived)
			rebuilt := newTestProgram(testCase.files, options, finalRoots)
			assertProgramsEquivalent(t, derived, rebuilt)
			assert.Equal(t, programFingerprint(base), before, "base program mutated after comparison")
		})
	}
}

// TestRemoveRootFilesRefusesADroppedDuplicate covers a root list that names the same
// file twice and drops one of the two. The file is not leaving the program — only one
// of its reasons for being there is — and reading the list as a removal would take it
// away entirely.
func TestRemoveRootFilesRefusesADroppedDuplicate(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	base := newTestProgram(files, options, []string{"/p/a.ts", "/p/b.ts", "/p/a.ts"})
	base.verifyCompilerOptions()
	base.CommonSourceDirectory()

	_, _, ok := base.UpdateRootFiles(newTestConfig(options, []string{"/p/a.ts", "/p/b.ts"}), nil, base.Host(), nil)
	assert.Assert(t, !ok)

	// and the other way round: the same file named a second time is an addition that
	// takes nothing away
	added, _, ok := base.UpdateRootFiles(newTestConfig(options, []string{"/p/a.ts", "/p/b.ts", "/p/a.ts", "/p/b.ts"}), nil, base.Host(), nil)
	assert.Assert(t, ok)
	assertProgramsEquivalent(t, added, newTestProgram(files, options, []string{"/p/a.ts", "/p/b.ts", "/p/a.ts", "/p/b.ts"}))
}

// TestRemoveRootFilesWithChangedFiles covers what the ts-morph loop actually does:
// a root leaving, a root arriving, and a file being edited, all in the same update.
func TestRemoveRootFilesWithChangedFiles(t *testing.T) {
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
	edited["/p/b.ts"] = "export const b = 2;\nclass B {}\n"
	delete(edited, "/p/a.ts")

	derived, _, ok := base.UpdateRootFiles(
		newTestConfig(options, []string{"/p/b.ts", "/p/c.ts"}),
		[]tspath.Path{"/p/b.ts"},
		newTestHost(edited),
		nil,
	)
	assert.Assert(t, ok)
	assert.Equal(t, programFingerprint(base), before, "base program mutated")
	assert.Equal(t, derived.GetSourceFileByPath("/p/b.ts").Text(), edited["/p/b.ts"])
	assert.Assert(t, derived.GetSourceFileByPath("/p/a.ts") == nil, "the removed file is still in the program")
	assertChangedPathsAreTheDiff(t, base, derived)
	assertProgramsEquivalent(t, derived, newTestProgram(edited, options, []string{"/p/b.ts", "/p/c.ts"}))
}

// TestRemoveRootFilesRolling is the loop the whole thing exists for: a file created
// and a file dropped on every step, against a program that never gets built again.
func TestRemoveRootFilesRolling(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{}
	var roots []string
	for i := range 6 {
		name := fmt.Sprintf("/p/b%d.ts", i)
		files[name] = fmt.Sprintf("export const v%d = %d;", i, i)
		roots = append(roots, name)
	}
	options := &core.CompilerOptions{ConfigFilePath: "/p/tsconfig.json"}
	program := newTestProgram(files, options, roots)
	program.verifyCompilerOptions()
	program.CommonSourceDirectory()

	for i := range 6 {
		added := fmt.Sprintf("/p/c%d.ts", i)
		files[added] = fmt.Sprintf("export const w%d = %d;", i, i)
		delete(files, roots[0])
		roots = slices.Concat(roots[1:], []string{added})

		next, _, ok := program.UpdateRootFiles(newTestConfig(options, roots), nil, newTestHost(files), nil)
		assert.Assert(t, ok, "UpdateRootFiles step %d", i)
		assertChangedPathsAreTheDiff(t, program, next)
		assertProgramsEquivalent(t, next, newTestProgram(files, options, roots))
		program = next
	}
}

// removeRootsPkgPaths maps a bare specifier onto a file in the project, so that an
// import of it resolves to a root rather than to the copy under node_modules.
var removeRootsPkgPaths = func() *collections.OrderedMap[string, []string] {
	m := &collections.OrderedMap[string, []string]{}
	m.Set("pkg", []string{"./pkg.ts"})
	return m
}()
