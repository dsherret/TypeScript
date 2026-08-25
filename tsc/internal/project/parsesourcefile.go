package project

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

// ParseSourceFile parses text as a source file through the parse cache a program build
// reads, so that the next program to hold that text finds the tree already there rather
// than parsing the same string a second time.
//
// This is the server half of a syntactic edit: a client that has just rewritten a file's
// text wants the tree back, and the program it asks its next semantic question of wants
// the same tree. Without this the text is parsed twice — once here and once when the
// program is built — which is what made an edit followed by a question cost more than the
// two separately.
//
// What makes the reuse sound is that this does not build a tree of its own. The value is
// produced by the cache, from the key, by the same Acquire call compilerHost.GetSourceFile
// would have made, so the only way the two could differ is if the key failed to cover
// something the parse depends on — and the key is the one the compiler already trusts to
// share one tree between programs, projects and snapshots.
//
// It is the *key* the caller has to get right, and the way it can be got wrong is benign.
// An entry filed under a key no build asks for is never found: the build parses the text
// itself, as it did before, and the entry goes when the offer is released. Nothing is
// answered from a tree parsed under different assumptions, because the assumptions are
// what the key is made of — the file name, its path, its external module indicator
// options, its script kind and the hash of its content. A hit means all five agreed, and
// five agreeing is the whole of what parser.ParseSourceFile reads.
//
// The tree is retained until the session's next snapshot — see releaseOfferedFiles — and
// at most one is retained per path.
func (s *Session) ParseSourceFile(opts ast.SourceFileParseOptions, text string) *ast.SourceFile {
	// The key is built out of the file handle rather than beside it, which is what
	// compilerHost.GetSourceFile does — so the two cannot drift, and the text is hashed
	// once. A path held open as an overlay is the one case the handles differ: an overlay's
	// script kind is the language id the client opened it with rather than its extension,
	// and where the two disagree this misses.
	fh := newDiskFile(opts.FileName, text)
	key := NewParseCacheKey(opts, fh.Hash(), fh.Kind())
	file := s.parseCache.Acquire(key, fh)
	s.retainOfferedFile(opts.Path, key)
	return file
}

// retainOfferedFile records the reference ParseSourceFile took, dropping whatever it was
// holding for the same path.
func (s *Session) retainOfferedFile(path tspath.Path, key ParseCacheKey) {
	s.offeredFilesMu.Lock()
	if s.offeredFiles == nil {
		s.offeredFiles = make(map[tspath.Path]ParseCacheKey)
	}
	previous, had := s.offeredFiles[path]
	s.offeredFiles[path] = key
	s.offeredFilesMu.Unlock()
	// the same text offered twice holds two references on one entry, so there is one to
	// drop either way
	if had {
		s.parseCache.Deref(previous)
	}
}

// releaseOfferedFiles lets go of the trees ParseSourceFile retained.
//
// A snapshot is where the reference was being held for: building its programs is when a
// parse cache lookup happens, so after one has been built the offer has either been taken
// — in which case the program holds a reference of its own and the tree stays — or it was
// keyed for a build that did not come. Either way nothing else is waiting for it.
//
// This is what bounds what an editing loop can retain: the trees offered since the last
// snapshot, one per path, each of them a tree the client is holding anyway.
func (s *Session) releaseOfferedFiles() {
	s.offeredFilesMu.Lock()
	offered := s.offeredFiles
	s.offeredFiles = nil
	s.offeredFilesMu.Unlock()
	for _, key := range offered {
		s.parseCache.Deref(key)
	}
}
