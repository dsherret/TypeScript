package api

import (
	"context"
	"strings"
	"testing"

	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
	"gotest.tools/v3/assert"
)

// A client can only otherwise set the quote preference for a whole snapshot, so
// getCodeFixes and getCombinedCodeFix take one per request. With none supplied
// the fixer infers the quotes from the file's own string literals.

func TestGetCombinedCodeFixHonoursQuotePreference(t *testing.T) {
	t.Parallel()
	assert.Equal(t, combinedImportFixText(t, "single"), `import { Test } from './Test';`)
	assert.Equal(t, combinedImportFixText(t, "double"), `import { Test } from "./Test";`)
}

// "auto" and an empty preference both leave the fixer to infer from the file. The
// fixture has no import to copy, so both fall back to double quotes — this pins
// that an unset preference is not silently read as "single".
func TestGetCombinedCodeFixInfersQuotesWhenNoPreference(t *testing.T) {
	t.Parallel()
	assert.Equal(t, combinedImportFixText(t, ""), `import { Test } from "./Test";`)
	assert.Equal(t, combinedImportFixText(t, "auto"), `import { Test } from "./Test";`)
}

func TestGetCombinedCodeFixRejectsUnknownQuotePreference(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}
	session, snapshot, projectID, fileName := setupImportFixProject(t)
	_, err := session.handleGetCombinedCodeFix(context.Background(), &GetCombinedCodeFixParams{
		Snapshot:        snapshot,
		Project:         projectID,
		File:            DocumentIdentifier{FileName: fileName},
		FixId:           "fixMissingImport",
		QuotePreference: "backtick",
	})
	assert.ErrorContains(t, err, "unknown quote preference")
}

func TestGetCodeFixesHonoursQuotePreference(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	for preference, expected := range map[string]string{
		"single": `import { Test } from './Test';`,
		"double": `import { Test } from "./Test";`,
	} {
		session, snapshot, projectID, fileName := setupImportFixProject(t)
		fixes, err := session.handleGetCodeFixes(context.Background(), &GetCodeFixesParams{
			Snapshot:        snapshot,
			Project:         projectID,
			File:            DocumentIdentifier{FileName: fileName},
			Pos:             0,
			End:             len(importFixMain),
			ErrorCodes:      []int{2304},
			QuotePreference: preference,
		})
		assert.NilError(t, err)
		assert.Assert(t, len(fixes) > 0, "expected an import fix for %s quotes", preference)
		assert.Equal(t, firstEditText(t, fixes[0].Changes), expected)
	}
}

// A single unresolved name, and no string literal anywhere for the fixer to infer
// quotes from — so the preference is the only thing that can decide them. The
// binding is not called `test`, or the checker suggests it (2552) instead of
// reporting an unresolved name (2304).
const importFixMain = "const value = new Test();\n"

func combinedImportFixText(t *testing.T, preference string) string {
	t.Helper()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	session, snapshot, projectID, fileName := setupImportFixProject(t)
	combined, err := session.handleGetCombinedCodeFix(context.Background(), &GetCombinedCodeFixParams{
		Snapshot:        snapshot,
		Project:         projectID,
		File:            DocumentIdentifier{FileName: fileName},
		FixId:           "fixMissingImport",
		QuotePreference: preference,
	})
	assert.NilError(t, err)
	assert.Assert(t, combined != nil, "expected a combined fix")
	return firstEditText(t, combined.Changes)
}

// firstEditText is the inserted import statement, without the trailing blank lines
// the fixer adds to separate it from the code.
func firstEditText(t *testing.T, changes []*FileTextEdits) string {
	t.Helper()
	assert.Assert(t, len(changes) == 1, "expected edits for exactly one file")
	assert.Assert(t, len(changes[0].Edits) > 0, "expected at least one edit")
	return strings.TrimRight(changes[0].Edits[0].NewText, "\r\n")
}

func setupImportFixProject(t *testing.T) (*Session, SnapshotID, ProjectID, string) {
	t.Helper()
	const fileName = "/home/projects/p/main.ts"
	files := map[string]any{
		"/home/projects/p/tsconfig.json": `{ "compilerOptions": { "strict": true } }`,
		"/home/projects/p/Test.ts":       "export class Test {}",
		fileName:                         importFixMain,
	}

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

	return session, snapshotResp.Snapshot, proj.Id, fileName
}
