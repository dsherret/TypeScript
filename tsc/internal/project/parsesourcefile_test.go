package project

import (
	"testing"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestParseSourceFileOfferLifetime pins what Session.ParseSourceFile holds and for how
// long, which is the whole of what seeding the parse cache can cost: an entry nothing
// takes is one AST that would otherwise have been collected.
//
// The bound is one entry per path, released at the next snapshot. A run of edits that
// reads nothing therefore holds one tree per file it touched — each of them a tree the
// client is holding too — and never a version per edit.
func TestParseSourceFileOfferLifetime(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.ts"
	session, fs := newAPIRootsSession(t, map[string]string{fileName: "export const a = 1;\n"}, []string{fileName})
	defer addRootsClose(t, session)

	const first = "export const a = 1;\nexport class C {}\n"
	firstKey := parseSourceFileForTest(t, session, fileName, first)
	assert.Equal(t, parseCacheRefCount(session, firstKey), 1)

	// offering the same text again is still one entry held once, not one per edit
	assert.Equal(t, parseSourceFileForTest(t, session, fileName, first), firstKey)
	assert.Equal(t, parseCacheRefCount(session, firstKey), 1)

	// and a further edit of the same path replaces what the path was holding
	const second = "export const a = 1;\nexport class C {}\nexport class D {}\n"
	secondKey := parseSourceFileForTest(t, session, fileName, second)
	assert.Equal(t, parseCacheRefCount(session, firstKey), 0)
	assert.Equal(t, parseCacheRefCount(session, secondKey), 1)

	// the snapshot that picks the text up takes the tree over: the reference the offer
	// held is gone and the program's is what keeps it
	assert.NilError(t, fs.WriteFile(fileName, second))
	var changes FileChangeSummary
	changes.Changed.Add(addRootsURI(fileName))
	apiRootsUpdate(t, session, changes, &APIRootFileChange{})
	assert.Equal(t, parseCacheRefCount(session, secondKey), 1)
	assert.Equal(t, addRootsProgram(t, session).GetSourceFileByPath(tspath.Path(fileName)).Text(), second)

	// an offer no snapshot had a use for is not left behind
	strayKey := parseSourceFileForTest(t, session, "/p/stray.ts", "export const stray = 1;\n")
	assert.Equal(t, parseCacheRefCount(session, strayKey), 1)
	apiRootsUpdate(t, session, FileChangeSummary{}, &APIRootFileChange{})
	assert.Equal(t, parseCacheRefCount(session, strayKey), 0)
	assert.Equal(t, offeredFileCount(session), 0)
}

// parseSourceFileForTest parses text the way the API session does, with the parse options
// a file with no package scope of its own gets, and reports the key it was filed under.
func parseSourceFileForTest(t *testing.T, session *Session, fileName string, text string) ParseCacheKey {
	t.Helper()
	file := session.ParseSourceFile(ast.SourceFileParseOptions{FileName: fileName, Path: tspath.Path(fileName)}, text)
	assert.Equal(t, file.Text(), text)
	return NewParseCacheKey(file.ParseOptions(), file.Hash, file.ScriptKind)
}

// parseCacheRefCount is how many references the cache holds for a key, and 0 for a key it
// holds no entry for — which is the same thing, since an entry at zero is evicted.
func parseCacheRefCount(session *Session, key ParseCacheKey) int {
	entry, ok := session.parseCache.entries.Load(key)
	if !ok {
		return 0
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.refCount
}

// offeredFileCount is how many trees Session.ParseSourceFile is still holding.
func offeredFileCount(session *Session) int {
	session.offeredFilesMu.Lock()
	defer session.offeredFilesMu.Unlock()
	return len(session.offeredFiles)
}
