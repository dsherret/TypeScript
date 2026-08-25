package project

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

// TestAPIRootsProjectLevelRepeated is TestAddRootsProjectLevelRepeated with the roots
// named for the project over the API instead of written into its config: the same
// program every time, added to rather than rebuilt, and the same parse cache ledger.
// The config is never touched, which is the whole point — nothing the loop writes grows
// with the project.
func TestAPIRootsProjectLevelRepeated(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{"/p/a.ts": `export const a = 1;`}
	roots := []string{"/p/a.ts"}
	session, fs := newAPIRootsSession(t, files, roots)

	for i := range 5 {
		name := fmt.Sprintf("/p/f%d.ts", i)
		text := fmt.Sprintf("import { a } from %q;\nexport const f%d = a;\n", "./a", i)
		assert.NilError(t, fs.WriteFile(name, text))
		files[name] = text
		roots = append(roots, name)

		var changes FileChangeSummary
		changes.Created.Add(addRootsURI(name))
		changes.IncludesWatchChangeOutsideNodeModules = true
		before := addRootsProgram(t, session).GetIncludeReasons()
		apiRootsUpdate(t, session, changes, &APIRootFileChange{Added: []string{name}})
		program := addRootsProgram(t, session)
		assert.Assert(t, addRootsReusedProgram(before, program.GetIncludeReasons()), "added root %s", name)

		atOnce, _ := newAPIRootsSession(t, files, roots)
		assert.Equal(t, addRootsExplain(program), addRootsExplain(addRootsProgram(t, atOnce)), "explained files after %s", name)
		addRootsAssertRefCounts(t, session, program)
		addRootsClose(t, atOnce)
	}

	// the config the project was opened with was written once and never again
	assert.Equal(t, addRootsProgram(t, session).CommandLine().ConfigName(), addRootsConfigFile)
	addRootsClose(t, session)
}

// TestAPIRootsProjectLevelRemoval checks that dropping a root takes its file out of the
// program and leaves the parse cache holding nothing for it.
func TestAPIRootsProjectLevelRemoval(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
	}
	session, _ := newAPIRootsSession(t, files, []string{"/p/a.ts", "/p/b.ts"})
	apiRootsUpdate(t, session, FileChangeSummary{}, &APIRootFileChange{Removed: []string{"/p/b.ts"}})

	program := addRootsProgram(t, session)
	assert.Assert(t, program.GetSourceFileByPath("/p/b.ts") == nil)
	assert.Assert(t, program.GetSourceFileByPath("/p/a.ts") != nil)
	addRootsAssertRefCounts(t, session, program)
	addRootsClose(t, session)
}

// TestAPIRootsProjectLevelRolling is the loop this whole thing exists for: a file
// created and a file dropped on every step, over the API, against a program that is
// never built again. It checks the program against one opened with the same files
// every step, and the parse cache ledger with it — a removal that let go of a file the
// program still holds, or held on to one it does not, would show up there rather than
// in what the program says.
func TestAPIRootsProjectLevelRolling(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{}
	var roots []string
	for i := range 4 {
		name := fmt.Sprintf("/p/b%d.ts", i)
		files[name] = fmt.Sprintf("export const v%d = %d;", i, i)
		roots = append(roots, name)
	}
	session, fs := newAPIRootsSession(t, files, roots)

	for i := range 6 {
		added := fmt.Sprintf("/p/c%d.ts", i)
		text := fmt.Sprintf("export const w%d = %d;", i, i)
		assert.NilError(t, fs.WriteFile(added, text))
		files[added] = text
		dropped := roots[0]
		assert.NilError(t, fs.Remove(dropped))
		delete(files, dropped)
		roots = slices.Concat(roots[1:], []string{added})

		var changes FileChangeSummary
		changes.Created.Add(addRootsURI(added))
		changes.Deleted.Add(addRootsURI(dropped))
		changes.IncludesWatchChangeOutsideNodeModules = true
		before := addRootsProgram(t, session).GetIncludeReasons()
		apiRootsUpdate(t, session, changes, &APIRootFileChange{Added: []string{added}, Removed: []string{dropped}})

		program := addRootsProgram(t, session)
		assert.Assert(t, addRootsReusedProgram(before, program.GetIncludeReasons()), "step %d", i)
		assert.Assert(t, program.GetSourceFileByPath(tspath.Path(dropped)) == nil, "step %d kept %s", i, dropped)
		assert.Assert(t, program.GetSourceFileByPath(tspath.Path(added)) != nil, "step %d missed %s", i, added)

		atOnce, _ := newAPIRootsSession(t, files, roots)
		assert.Equal(t, addRootsExplain(program), addRootsExplain(addRootsProgram(t, atOnce)), "explained files at step %d", i)
		addRootsAssertRefCounts(t, session, program)
		addRootsClose(t, atOnce)
	}
	addRootsClose(t, session)
}

func newAPIRootsSession(t *testing.T, files map[string]string, roots []string) (*Session, *addRootsVFS) {
	t.Helper()
	initial := map[string]any{addRootsConfigFile: apiRootsConfigJSON()}
	for name, text := range files {
		initial[name] = text
	}
	vfs := vfstest.FromMap(initial, true /*useCaseSensitiveFileNames*/)
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
	apiRootsUpdate(t, session, FileChangeSummary{}, &APIRootFileChange{Added: slices.Clone(roots)})
	return session, &addRootsVFS{WriteFile: vfs.WriteFile, Remove: vfs.Remove}
}

func apiRootsUpdate(t *testing.T, session *Session, changes FileChangeSummary, rootFiles *APIRootFileChange) {
	t.Helper()
	openProjects := collections.NewSetWithSizeHint[string](1)
	openProjects.Add(addRootsConfigFile)
	snapshot, err := session.APIUpdate(context.Background(), changes, &APISnapshotRequest{
		OpenProjects: openProjects,
		RootFiles:    map[tspath.Path]*APIRootFileChange{tspath.Path(addRootsConfigFile): rootFiles},
	})
	assert.NilError(t, err)
	snapshot.Deref(session)
}

// apiRootsConfigJSON is the config a client that names its own roots writes: the same
// options as addRootsConfigJSON, and a `files` list that is present so the default
// `**/*` include stays off, and empty because the roots do not come from here.
func apiRootsConfigJSON() string {
	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{
			"allowJs": true,
			"baseUrl": ".",
			"paths":   map[string]any{"@app/*": []string{"./src/*"}},
			"outDir":  "./out",
			"rootDir": ".",
		},
		"files": []string{},
	})
	return string(text)
}
