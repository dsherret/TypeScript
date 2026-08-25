package project

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

const addRootsConfigFile = "/p/tsconfig.json"

// TestAddRootsProjectLevel drives the project layer the way ts-morph does — write a
// file, name it in the config, ask for a snapshot — and checks three things for each
// case: whether the previous program was added to or built again, that the program
// says what one built with every file from the start says, and that the parse cache
// is left holding nothing once the project is closed.
func TestAddRootsProjectLevel(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name          string
		caseSensitive bool
		initial       map[string]string
		added         map[string]string
		// alsoChanged are files already in the project whose text changed in the
		// same batch as the addition.
		alsoChanged map[string]string
		alsoDeleted []string
		// alsoUnrooted are files dropped from the config's file list and left on the
		// file system, where alsoDeleted are removed from both.
		alsoUnrooted  []string
		reusesProgram bool
	}{
		{
			name:          "a file nothing was waiting for",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         map[string]string{"/p/b.ts": `export const b = 2;`},
			reusesProgram: true,
		},
		{
			name:          "a file an import was waiting for",
			initial:       map[string]string{"/p/a.ts": `import { b } from "./b"; export const a = b;`},
			added:         map[string]string{"/p/b.ts": `export const b = 2;`},
			reusesProgram: false,
		},
		{
			name:          "a file an import through paths was waiting for",
			initial:       map[string]string{"/p/a.ts": `import { b } from "@app/b"; export const a = b;`},
			added:         map[string]string{"/p/src/b.ts": `export const b = 2;`},
			reusesProgram: false,
		},
		{
			name:          "a file an import of a directory index was waiting for",
			initial:       map[string]string{"/p/a.ts": `import { d } from "./dir"; export const a = d;`},
			added:         map[string]string{"/p/dir/index.ts": `export const d = 2;`},
			reusesProgram: false,
		},
		{
			name:          "a file a type reference was waiting for",
			initial:       map[string]string{"/p/a.ts": "/// <reference types=\"thing\" />\nexport const a = 1;"},
			added:         map[string]string{"/p/node_modules/@types/thing/index.d.ts": `declare const thing: number;`},
			reusesProgram: false,
		},
		{
			name:          "a file created inside node_modules",
			initial:       map[string]string{"/p/a.ts": `import { p } from "pkg"; export const a = p;`},
			added:         map[string]string{"/p/node_modules/pkg/index.d.ts": `export declare const p: number;`},
			reusesProgram: false,
		},
		{
			name:          "two files changing in the batch the addition arrives in",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/b.ts": `export const b = 2;`},
			added:         map[string]string{"/p/c.ts": `export const c = 3;`},
			alsoChanged:   map[string]string{"/p/a.ts": "export const a = 1;\nclass A {}\n", "/p/b.ts": "export const b = 2;\nclass B {}\n"},
			reusesProgram: true,
		},
		{
			name:          "a changed file whose imports changed in the batch the addition arrives in",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			added:         map[string]string{"/p/c.ts": `export const c = 3;`},
			alsoChanged:   map[string]string{"/p/a.ts": `import { dep } from "./dep"; export const a = dep;`},
			reusesProgram: false,
		},
		{
			name:          "a package.json changing in the batch the addition arrives in",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/package.json": `{"name":"p","version":"1.0.0"}`},
			added:         map[string]string{"/p/b.ts": `export const b = 2;`},
			alsoChanged:   map[string]string{"/p/package.json": `{"name":"p","version":"2.0.0"}`},
			reusesProgram: false,
		},
		{
			name:          "a file deleted in the batch the addition arrives in",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			added:         map[string]string{"/p/b.ts": `export const b = 2;`},
			alsoDeleted:   []string{"/p/dep.ts"},
			reusesProgram: true,
		},
		{
			name:          "a file deleted on its own",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			alsoDeleted:   []string{"/p/dep.ts"},
			reusesProgram: true,
		},
		{
			name:          "a file deleted that another file imports",
			initial:       map[string]string{"/p/a.ts": `import { dep } from "./dep"; export const a = dep;`, "/p/dep.ts": `export const dep = 3;`},
			alsoDeleted:   []string{"/p/dep.ts"},
			reusesProgram: false,
		},
		{
			// the file is gone from the file system too, so the arriving import
			// resolves to nothing and the walk never reaches what is leaving
			name:          "a file deleted while a file that imports it arrives",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			added:         map[string]string{"/p/b.ts": `import { dep } from "./dep"; export const b = dep;`},
			alsoDeleted:   []string{"/p/dep.ts"},
			reusesProgram: true,
		},
		{
			name:          "a file dropped from the roots and left where it is",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			alsoUnrooted:  []string{"/p/dep.ts"},
			reusesProgram: true,
		},
		{
			// this one the arriving import does reach, and what it means there is a
			// question about the whole file system rather than about the program
			name:          "a file dropped from the roots while a file that imports it arrives",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			added:         map[string]string{"/p/b.ts": `import { dep } from "./dep"; export const b = dep;`},
			alsoUnrooted:  []string{"/p/dep.ts"},
			reusesProgram: false,
		},
		{
			name:          "a file deleted while another is edited and a third arrives",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`, "/p/dep.ts": `export const dep = 3;`},
			added:         map[string]string{"/p/b.ts": `export const b = 2;`},
			alsoChanged:   map[string]string{"/p/a.ts": "export const a = 1;\nclass A {}\n"},
			alsoDeleted:   []string{"/p/dep.ts"},
			reusesProgram: true,
		},
		{
			name:          "a file deleted that a declaration file shares a stem with",
			initial:       map[string]string{"/p/a.ts": `import { s } from "./s"; export const a = s;`, "/p/s.ts": `export const s = 1;`, "/p/s.d.ts": `export declare const s: string;`},
			alsoDeleted:   []string{"/p/s.d.ts"},
			reusesProgram: true,
		},
		{
			name:          "the implementation a declaration file shares a stem with",
			initial:       map[string]string{"/p/a.ts": `import { s } from "./s"; export const a = s;`, "/p/s.ts": `export const s = 1;`, "/p/s.d.ts": `export declare const s: string;`},
			alsoDeleted:   []string{"/p/s.ts"},
			reusesProgram: false,
		},
		{
			name:          "a file deleted that a package under node_modules would be found instead of",
			initial:       map[string]string{"/p/a.ts": `import { p } from "pkg"; export const a = p;`, "/p/node_modules/pkg/package.json": `{"name":"pkg","version":"1.0.0","types":"index.d.ts"}`, "/p/node_modules/pkg/index.d.ts": `export declare const p: number;`, "/p/other.ts": `export const other = 1;`},
			alsoDeleted:   []string{"/p/other.ts"},
			reusesProgram: true,
		},
		{
			name:          "a file reached under two casings where casing does not distinguish files",
			caseSensitive: false,
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added: map[string]string{
				"/p/b.ts":     "import { u } from \"./utils\";\nimport { u as v } from \"./UTILS\";\nexport const b = u + v;",
				"/p/utils.ts": `export const u = 1;`,
			},
			reusesProgram: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			initial := maps2Clone(testCase.initial)
			roots := addRootsRootNames(initial)
			session, fs := newAddRootsSession(t, initial, roots, testCase.caseSensitive)
			before := addRootsProgram(t, session).GetIncludeReasons()

			var changes FileChangeSummary
			all := maps2Clone(initial)
			for name, text := range testCase.added {
				assert.NilError(t, fs.WriteFile(name, text))
				all[name] = text
				changes.Created.Add(addRootsURI(name))
			}
			for name, text := range testCase.alsoChanged {
				assert.NilError(t, fs.WriteFile(name, text))
				all[name] = text
				changes.Changed.Add(addRootsURI(name))
			}
			for _, name := range testCase.alsoDeleted {
				assert.NilError(t, fs.Remove(name))
				delete(all, name)
				changes.Deleted.Add(addRootsURI(name))
			}
			roots = append(roots, addRootsRootNames(testCase.added)...)
			roots = slices.DeleteFunc(roots, func(name string) bool {
				return slices.Contains(testCase.alsoDeleted, name) || slices.Contains(testCase.alsoUnrooted, name)
			})
			assert.NilError(t, fs.WriteFile(addRootsConfigFile, addRootsConfigJSON(roots)))
			changes.Changed.Add(addRootsURI(addRootsConfigFile))
			changes.IncludesWatchChangeOutsideNodeModules = true
			addRootsUpdate(t, session, changes)

			program := addRootsProgram(t, session)
			assert.Equal(t, addRootsReusedProgram(before, program.GetIncludeReasons()), testCase.reusesProgram, "reused the program")
			for name, text := range testCase.alsoChanged {
				if file := program.GetSourceFileByPath(tspath.Path(name)); file != nil {
					assert.Equal(t, file.Text(), text, "text of %s", name)
				}
			}

			atOnce, _ := newAddRootsSession(t, all, roots, testCase.caseSensitive)
			assert.Equal(t, addRootsExplain(program), addRootsExplain(addRootsProgram(t, atOnce)), "explained files")

			// the parse cache must hold exactly one reference per file, whether the
			// program was added to or built again after a walk that gave up
			addRootsAssertRefCounts(t, session, program)
			addRootsClose(t, session)
			addRootsClose(t, atOnce)
		})
	}
}

// TestAddRootsProjectLevelRepeated adds a root over and over, which is the loop the
// whole change exists for, and checks the program against a rebuild every time.
func TestAddRootsProjectLevelRepeated(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{"/p/a.ts": `export const a = 1;`}
	roots := []string{"/p/a.ts"}
	session, fs := newAddRootsSession(t, files, roots, true)

	for i := range 5 {
		name := fmt.Sprintf("/p/f%d.ts", i)
		text := fmt.Sprintf("import { a } from %q;\nexport const f%d = a;\n", "./a", i)
		assert.NilError(t, fs.WriteFile(name, text))
		files[name] = text
		roots = append(roots, name)
		assert.NilError(t, fs.WriteFile(addRootsConfigFile, addRootsConfigJSON(roots)))

		var changes FileChangeSummary
		changes.Created.Add(addRootsURI(name))
		changes.Changed.Add(addRootsURI(addRootsConfigFile))
		changes.IncludesWatchChangeOutsideNodeModules = true
		before := addRootsProgram(t, session).GetIncludeReasons()
		addRootsUpdate(t, session, changes)
		program := addRootsProgram(t, session)
		assert.Assert(t, addRootsReusedProgram(before, program.GetIncludeReasons()), "added root %s", name)

		atOnce, _ := newAddRootsSession(t, files, roots, true)
		assert.Equal(t, addRootsExplain(program), addRootsExplain(addRootsProgram(t, atOnce)), "explained files after %s", name)
		addRootsAssertRefCounts(t, session, program)
		addRootsClose(t, atOnce)
	}
	addRootsClose(t, session)
}

type addRootsVFS struct {
	WriteFile func(path string, content string) error
	Remove    func(path string) error
}

func newAddRootsSession(t *testing.T, files map[string]string, roots []string, caseSensitive bool) (*Session, *addRootsVFS) {
	t.Helper()
	initial := map[string]any{}
	for name, text := range files {
		initial[name] = text
	}
	initial[addRootsConfigFile] = addRootsConfigJSON(roots)
	vfs := vfstest.FromMap(initial, caseSensitive)
	session := NewSession(&SessionInit{
		BackgroundCtx: context.Background(),
		Options: &SessionOptions{
			CurrentDirectory:   "/",
			DefaultLibraryPath: bundled.LibPath(),
			PositionEncoding:   lsproto.PositionEncodingKindUTF8,
		},
		FS:     bundled.WrapFS(vfs),
		Client: addRootsClient{},
	})
	addRootsUpdate(t, session, FileChangeSummary{})
	return session, &addRootsVFS{WriteFile: vfs.WriteFile, Remove: vfs.Remove}
}

func addRootsUpdate(t *testing.T, session *Session, changes FileChangeSummary) {
	t.Helper()
	openProjects := collections.NewSetWithSizeHint[string](1)
	openProjects.Add(addRootsConfigFile)
	snapshot, err := session.APIUpdate(context.Background(), changes, &APISnapshotRequest{OpenProjects: openProjects})
	assert.NilError(t, err)
	snapshot.Deref(session)
}

func addRootsConfigJSON(roots []string) string {
	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{
			"allowJs": true,
			"baseUrl": ".",
			"paths":   map[string]any{"@app/*": []string{"./src/*"}},
			"outDir":  "./out",
			"rootDir": ".",
		},
		"files": roots,
	})
	return string(text)
}

func addRootsURI(fileName string) lsproto.DocumentUri {
	return lsproto.DocumentUri("file://" + fileName)
}

func addRootsRootNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		if strings.HasSuffix(name, ".ts") && !strings.Contains(name, "/node_modules/") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func maps2Clone(files map[string]string) map[string]string {
	clone := make(map[string]string, len(files))
	for name, text := range files {
		clone[name] = text
	}
	return clone
}

func addRootsProgram(t *testing.T, session *Session) *compiler.Program {
	t.Helper()
	configured := session.Snapshot().ProjectCollection.ConfiguredProject(tspath.Path(addRootsConfigFile))
	assert.Assert(t, configured != nil)
	return configured.GetProgram()
}

// addRootsReusedProgram reports whether the second program carried the first's
// include reasons, which only a program built by adding to it can have.
func addRootsReusedProgram(before map[tspath.Path][]*compiler.FileIncludeReason, after map[tspath.Path][]*compiler.FileIncludeReason) bool {
	for path, reasons := range before {
		if now, ok := after[path]; ok && len(now) > 0 && len(reasons) > 0 && now[0] == reasons[0] {
			return true
		}
	}
	return false
}

func addRootsExplain(program *compiler.Program) string {
	var b strings.Builder
	program.ExplainFiles(&b, locale.Default)
	return b.String()
}

// addRootsAssertRefCounts checks the parse cache ledger for the project's files: a
// live snapshot holds exactly one reference to each file its program holds and each
// duplicate it reports, and nothing else the project reached is left in the cache.
// One reference too many is a leak, one too few is a panic waiting to happen, and
// both are what a walk that acquired files and then gave up would leave behind.
func addRootsAssertRefCounts(t *testing.T, session *Session, program *compiler.Program) {
	t.Helper()
	session.WaitForBackgroundTasks()

	expected := map[ParseCacheKey]int{}
	for _, file := range program.SourceFiles() {
		if key := NewParseCacheKey(file.ParseOptions(), file.Hash, file.ScriptKind); strings.HasPrefix(key.FileName, "/p/") {
			expected[key]++
		}
	}
	for _, file := range program.DuplicateSourceFiles() {
		if key := NewParseCacheKey(file.ParseOptions, file.Hash, file.ScriptKind); strings.HasPrefix(key.FileName, "/p/") {
			expected[key]++
		}
	}

	var wrong []string
	seen := map[ParseCacheKey]struct{}{}
	session.parseCache.entries.Range(func(key ParseCacheKey, entry *refCountCacheEntry[*ast.SourceFile]) bool {
		if !strings.HasPrefix(key.FileName, "/p/") {
			return true
		}
		seen[key] = struct{}{}
		if want := expected[key]; entry.refCount != want {
			wrong = append(wrong, fmt.Sprintf("%s: %d references, expected %d", key.FileName, entry.refCount, want))
		}
		return true
	})
	for key, want := range expected {
		if _, ok := seen[key]; !ok {
			wrong = append(wrong, fmt.Sprintf("%s: no entry, expected %d references", key.FileName, want))
		}
	}
	slices.Sort(wrong)
	assert.Equal(t, strings.Join(wrong, "; "), "", "parse cache reference counts")
}

func addRootsClose(t *testing.T, session *Session) {
	t.Helper()
	closeProjects := collections.NewSetWithSizeHint[tspath.Path](1)
	closeProjects.Add(tspath.Path(addRootsConfigFile))
	snapshot, err := session.APIUpdate(context.Background(), FileChangeSummary{}, &APISnapshotRequest{CloseProjects: closeProjects})
	assert.NilError(t, err)
	snapshot.Deref(session)
	session.WaitForBackgroundTasks()
	session.Close()
}

// addRootsClient is a client that does nothing, which is all these tests need one
// for: a session with none panics as soon as it refreshes diagnostics.
type addRootsClient struct{}

func (addRootsClient) WatchFiles(ctx context.Context, id WatcherID, watchers []*lsproto.FileSystemWatcher) error {
	return nil
}
func (addRootsClient) UnwatchFiles(ctx context.Context, id WatcherID) error     { return nil }
func (addRootsClient) RefreshDiagnostics(ctx context.Context) error             { return nil }
func (addRootsClient) RefreshInlayHints(ctx context.Context) error              { return nil }
func (addRootsClient) RefreshCodeLens(ctx context.Context) error                { return nil }
func (addRootsClient) ProgressStart(message *diagnostics.Message, args ...any)  {}
func (addRootsClient) ProgressFinish(message *diagnostics.Message, args ...any) {}
func (addRootsClient) IsActive() bool                                           { return true }
func (addRootsClient) SetLocale(l string)                                       {}
func (addRootsClient) RegisterContentMapperExtensions(ctx context.Context, extensions []string) error { return nil }
func (addRootsClient) GetLocale() locale.Locale                                 { return locale.Default }

func (addRootsClient) PublishDiagnostics(ctx context.Context, params *lsproto.PublishDiagnosticsParams) error {
	return nil
}

func (addRootsClient) SendTelemetry(ctx context.Context, telemetry lsproto.TelemetryEvent) error {
	return nil
}
