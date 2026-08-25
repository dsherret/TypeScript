package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/projecttestutil"
	"gotest.tools/v3/assert"
)

// The API is findRenameLocations, not getRenameInfo, so it applies none of the
// checks tsserver uses to decide what an editor may offer. A caller that
// assembled the whole program owns every file in it, node_modules included, and
// a rename that quietly skipped those files would leave the caller's own files
// half-renamed.

func TestRenameSymbolDeclaredInNodeModules(t *testing.T) {
	t.Parallel()
	edits := renameAt(t, renameProjectFiles("node_modules"), renameValuePos, "v2", renameOutright())
	assert.DeepEqual(t, summarizeEdits(edits), []string{
		"/home/projects/p/main.ts: [9,12) => v2; [37,40) => v2",
		"/home/projects/p/node_modules/pkg/index.d.ts: [21,24) => v2",
	})
}

// The identical project with the package one directory over. It renames on both
// sides of the fix, so it pins the directory name as the only variable.
func TestRenameSymbolDeclaredOutsideNodeModules(t *testing.T) {
	t.Parallel()
	edits := renameAt(t, renameProjectFiles("vendor"), renameValuePos, "v2", renameOutright())
	assert.DeepEqual(t, summarizeEdits(edits), []string{
		"/home/projects/p/main.ts: [9,12) => v2; [37,40) => v2",
		"/home/projects/p/vendor/pkg/index.d.ts: [21,24) => v2",
	})
}

// Aliasing keeps the rename inside the import, so it never reaches the
// declaration under node_modules. That always worked; this pins it so a change
// in the alias handling cannot be read as a change in what may be renamed.
func TestRenameSymbolDeclaredInNodeModulesWithAliases(t *testing.T) {
	t.Parallel()
	edits := renameAt(t, renameProjectFiles("node_modules"), renameValuePos, "v2", renameViaAlias())
	assert.DeepEqual(t, summarizeEdits(edits), []string{
		"/home/projects/p/main.ts: [9,12) => val as v2; [37,40) => v2",
	})
}

// Renaming across node_modules is allowed, but the standard library still has to
// be off limits: its files are excluded from the search rather than blocking the
// rename, so the caller's own references are renamed and lib.*.d.ts is untouched.
func TestRenameSymbolDeclaredInStandardLibrary(t *testing.T) {
	t.Parallel()
	files := map[string]any{
		"/home/projects/p/tsconfig.json": `{ "compilerOptions": { "strict": true } }`,
		"/home/projects/p/main.ts":       "const arr: Array<number> = [];\n",
	}
	edits := renameAt(t, files, 11 /*Array*/, "Sequence", renameOutright())
	assert.DeepEqual(t, summarizeEdits(edits), []string{
		"/home/projects/p/main.ts: [11,16) => Sequence",
	})
}

const (
	renameMain = "import { val } from \"pkg\";\nconst y = val;\n"
	// Offset of `val` in the named import of renameMain.
	renameValuePos = 9
)

// ts-morph's default is to rename outright rather than to alias, which is what
// makes the aliased symbol - the one declared in the package - the rename target.
func renameOutright() *bool { return ptrTo(false) }

func renameViaAlias() *bool { return ptrTo(true) }

// renameProjectFiles is one package of one exported value, imported by main.ts,
// with the package's directory as the only knob.
func renameProjectFiles(packageDir string) map[string]any {
	return map[string]any{
		"/home/projects/p/tsconfig.json": fmt.Sprintf(
			`{ "compilerOptions": { "strict": true, "baseUrl": ".", "paths": { "pkg": ["./%s/pkg/index.d.ts"] } } }`,
			packageDir,
		),
		"/home/projects/p/" + packageDir + "/pkg/package.json": `{ "name": "pkg", "types": "index.d.ts" }`,
		"/home/projects/p/" + packageDir + "/pkg/index.d.ts":   "export declare const val: number;\n",
		"/home/projects/p/main.ts":                             renameMain,
	}
}

func renameAt(t *testing.T, files map[string]any, position int, newName string, useAliases *bool) []*FileTextEdits {
	t.Helper()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/home/projects/p/main.ts"
	projectSession, _ := projecttestutil.Setup(files)
	t.Cleanup(projectSession.Close)
	session := NewSession(projectSession, nil)
	t.Cleanup(session.Close)

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

	edits, err := session.handleRename(ctx, &RenameParams{
		Snapshot:            snapshotResp.Snapshot,
		Project:             proj.Id,
		File:                DocumentIdentifier{FileName: fileName},
		Position:            position,
		NewName:             newName,
		UseAliasesForRename: useAliases,
	})
	assert.NilError(t, err)
	return edits
}

// summarizeEdits renders one line per file, sorted, so that a missing file and a
// wrong replacement both show up in the same assertion.
func summarizeEdits(edits []*FileTextEdits) []string {
	lines := make([]string, 0, len(edits))
	for _, fileEdits := range edits {
		ordered := slices.Clone(fileEdits.Edits)
		slices.SortFunc(ordered, func(a, b *TextEdit) int { return a.Pos - b.Pos })
		spans := make([]string, 0, len(ordered))
		for _, edit := range ordered {
			spans = append(spans, fmt.Sprintf("[%d,%d) => %s", edit.Pos, edit.End, edit.NewText))
		}
		lines = append(lines, fileEdits.FileName+": "+strings.Join(spans, "; "))
	}
	slices.Sort(lines)
	return lines
}

func ptrTo[T any](value T) *T {
	return &value
}
