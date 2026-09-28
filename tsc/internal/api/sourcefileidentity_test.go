package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/api/encoder"
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/project"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/projecttestutil"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestGetSourceFileIdentityMatchesTheEncodedHeader is the whole of what the endpoint has
// to be true for.
//
// A client uses it in place of fetching the file: it keys its source file cache on the
// content hash and the parse options key, and today it reads those out of the header of
// the tree it fetched. Answering them on their own is only sound if they are the same two
// values — so this decodes them out of getSourceFile's own response, exactly as the client
// does, and compares. If they ever diverged the client would reuse a tree the program does
// not hold, and every node handle would index into the wrong AST without anything
// throwing.
func TestGetSourceFileIdentityMatchesTheEncodedHeader(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name string
		file string
		text string
	}{
		{"ts", "/p/a.ts", "export class A { m() { return 1; } }\n"},
		{"tsx", "/p/a.tsx", "export const A = () => <div>hi</div>;\n"},
		{"declaration", "/p/a.d.ts", "export declare const a: number;\n"},
		{"js", "/p/a.js", "export const a = 1;\n"},
		{"mts", "/p/a.mts", "export const a = 1;\n"},
		{"cts", "/p/a.cts", "export const a = 1;\n"},
		{"json", "/p/a.json", "{\"a\": 1}\n"},
		{"empty", "/p/a.ts", ""},
		{"syntax error", "/p/a.ts", "export class {{{\n"},
		{"non-ascii", "/p/a.ts", "export const \U0001F600 = \"é中文\";\n"},
		{"jsdoc", "/p/a.js", "/** @param {number} x */\nexport function f(x) { return x; }\n"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			s := newParseSession(t, map[string]string{testCase.file: testCase.text}, map[string]any{
				"allowJs": true, "jsx": "preserve", "resolveJsonModule": true,
			})
			defer s.close()

			assertIdentityMatchesGetSourceFile(t, s, testCase.file)
		})
	}
}

// TestBuildNodeIndexTableMatchesTheEncoder is the invariant this change leans on much
// harder than anything did before it.
//
// A node handle is an index into the table encoder.BuildNodeIndexTable walks, and the
// index it resolves against is whichever table was computed first — see
// GetOrComputeSourceFileData in EncodeSourceFile. Before, every Program#getSourceFile
// encoded the file, so the encoder's own walk almost always got there first. Now a file
// the client reuses is never encoded, and the standalone walk is what handles resolve
// against. If the two ever numbered a node differently, a handle would land on the wrong
// node with nothing to throw about it.
//
// The comment on BuildNodeIndexTable promises they agree. This checks it, node for node
// and by pointer, over the shapes most likely to break a parallel walk: JSDoc, which is
// visited after a node's children rather than as one of them; modifier lists, which are
// visited through a hook of their own; and empty node lists, which occupy an index while
// being no node at all.
func TestBuildNodeIndexTableMatchesTheEncoder(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		file string
		text string
	}{
		{"ts", "/p/a.ts", "export abstract class A {\n  private static readonly x = 1;\n  abstract m(): void;\n}\n"},
		{"tsx", "/p/a.tsx", "export const A = () => <div id=\"x\">{[1, 2].map(n => <b key={n}>{n}</b>)}</div>;\n"},
		{"declaration", "/p/a.d.ts", "export declare const a: number;\nexport default interface I { m(): void }\n"},
		{"jsdoc", "/p/a.js", "/** @param {number} x\n * @returns {string} */\nexport function f(x) { return String(x); }\n/** @type {number} */\nlet y;\n"},
		{"jsdoc on every member", "/p/b.js", "/** doc */\nclass C {\n  /** doc */\n  m() {}\n  /** doc */\n  n = 1;\n}\n"},
		{"empty lists", "/p/c.ts", "function f() {}\nclass C {}\nenum E {}\nconst x = [];\nf();\n"},
		{"json", "/p/a.json", "{\"a\": [1, 2], \"b\": {\"c\": null}}\n"},
		{"empty", "/p/a.ts", ""},
		{"syntax error", "/p/a.ts", "export class {{{\n"},
		{"non-ascii", "/p/a.ts", "export const \U0001F600 = \"é中文\";\n"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sourceFile := parser.ParseSourceFile(
				ast.SourceFileParseOptions{FileName: testCase.file, Path: tspath.Path(testCase.file)},
				testCase.text,
				core.GetScriptKindFromFileName(testCase.file),
			)
			// first, so the encoder's own walk is the one that gets cached and this reads
			// what encodeTree built rather than what a previous call left behind
			_, encoded, err := encoder.EncodeSourceFile(sourceFile)
			assert.NilError(t, err)
			built := encoder.BuildNodeIndexTable(sourceFile)

			assert.Equal(t, len(built.Nodes), len(encoded.Nodes))
			for i := range encoded.Nodes {
				assert.Equal(t, built.Nodes[i], encoded.Nodes[i], "node %d", i)
			}
		})
	}
}

// TestGetSourceFileIdentityUnderAPackageScope covers the one parse option a file's own
// text and name do not settle. `"type": "module"` forces module detection, which moves the
// parse options key — the field that exists so a client cannot reuse a tree parsed under
// different assumptions.
func TestGetSourceFileIdentityUnderAPackageScope(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.js"
	s := newParseSession(t, map[string]string{
		fileName:          "const a = 1;\n",
		"/p/package.json": `{"type":"module"}`,
	}, map[string]any{"allowJs": true, "module": "nodenext", "moduleResolution": "nodenext", "moduleDetection": "auto"})
	defer s.close()

	identity := assertIdentityMatchesGetSourceFile(t, s, fileName)
	// the scope is doing something, so the comparison above is not comparing two zeroes
	assert.Equal(t, identity.ParseOptionsKey, "2")
}

// TestGetSourceFileIdentityOfAFileTheProgramLacks answers nil, which is what getSourceFile
// answers with too — so a client that asked this first and stopped there did not skip an
// answer it would otherwise have got.
func TestGetSourceFileIdentityOfAFileTheProgramLacks(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newParseSession(t, map[string]string{"/p/a.ts": "export const a = 1;\n"}, nil)
	defer s.close()

	identity, err := s.session.handleGetSourceFileIdentity(context.Background(), &GetSourceFileParams{
		Snapshot: s.snapshot,
		Project:  s.projectID,
		File:     DocumentIdentifier{FileName: "/p/stray.ts"},
	})
	assert.NilError(t, err)
	assert.Assert(t, identity == nil)

	viaProgram, err := s.session.handleGetSourceFile(context.Background(), &GetSourceFileParams{
		Snapshot: s.snapshot,
		Project:  s.projectID,
		File:     DocumentIdentifier{FileName: "/p/stray.ts"},
	})
	assert.NilError(t, err)
	assert.Assert(t, viaProgram == nil)
}

// assertIdentityMatchesGetSourceFile checks the endpoint against the header of the file
// getSourceFile encodes, read the way the client reads it, and returns what it answered.
func assertIdentityMatchesGetSourceFile(t *testing.T, s *parseSession, fileName string) *SourceFileIdentity {
	t.Helper()
	identity, err := s.session.handleGetSourceFileIdentity(context.Background(), &GetSourceFileParams{
		Snapshot: s.snapshot,
		Project:  s.projectID,
		File:     DocumentIdentifier{FileName: fileName},
	})
	assert.NilError(t, err)
	assert.Assert(t, identity != nil)

	viaProgram, err := s.session.handleGetSourceFile(context.Background(), &GetSourceFileParams{
		Snapshot: s.snapshot,
		Project:  s.projectID,
		File:     DocumentIdentifier{FileName: fileName},
	})
	assert.NilError(t, err)
	data := decodeSourceFileResponse(t, viaProgram)
	assert.Equal(t, identity.ContentHash, headerContentHash(data))
	assert.Equal(t, identity.ParseOptionsKey, headerParseOptionsKey(data))
	return identity
}

// headerContentHash reads the hash out of an encoded source file the way
// readSourceFileHash does on the client.
func headerContentHash(data []byte) string {
	return fmt.Sprintf("%016x%016x",
		binary.LittleEndian.Uint64(data[encoder.HeaderOffsetHashHi0:]),
		binary.LittleEndian.Uint64(data[encoder.HeaderOffsetHashLo0:]))
}

// headerParseOptionsKey reads the parse options key out of an encoded source file the way
// readParseOptionsKey does on the client.
func headerParseOptionsKey(data []byte) string {
	return fmt.Sprintf("%d", binary.LittleEndian.Uint32(data[encoder.HeaderOffsetParseOptions:]))
}

const parseSessionConfigFileName = "/p/tsconfig.json"

// parseSession is a session with one configured project open, which is all these tests
// need: a snapshot to ask getSourceFileIdentity of, and a program to compare it with.
type parseSession struct {
	session   *Session
	project   *project.Session
	utils     *projecttestutil.SessionUtils
	snapshot  SnapshotID
	projectID project.ID
}

func newParseSession(t *testing.T, files map[string]string, compilerOptions map[string]any) *parseSession {
	t.Helper()
	if compilerOptions == nil {
		compilerOptions = map[string]any{}
	}
	roots := make([]string, 0, len(files))
	initial := map[string]any{}
	for name, text := range files {
		initial[name] = text
		if name != "/p/package.json" {
			roots = append(roots, name)
		}
	}
	configText, err := json.Marshal(map[string]any{"compilerOptions": compilerOptions, "files": roots})
	assert.NilError(t, err)
	initial[parseSessionConfigFileName] = string(configText)

	projectSession, utils := projecttestutil.SetupWithOptions(initial, &project.SessionOptions{
		CurrentDirectory:   "/",
		DefaultLibraryPath: bundled.LibPath(),
		PositionEncoding:   lsproto.PositionEncodingKindUTF8,
	})
	s := &parseSession{session: NewLSPSession(projectSession, nil), project: projectSession, utils: utils}
	response, err := s.session.handleCreateSnapshot(context.Background(), &CreateSnapshotParams{
		SnapshotRequestChangesParams: SnapshotRequestChangesParams{
			OpenProjects:   []DocumentIdentifier{{FileName: parseSessionConfigFileName}},
			EnsurePrograms: &EnsurePrograms{All: true},
		},
	})
	assert.NilError(t, err)
	assert.Assert(t, len(response.Projects) > 0)
	s.snapshot = response.Snapshot
	s.projectID = response.Projects[0].Id
	return s
}

// commit writes text where the compiler reads it and builds the snapshot that picks it up.
func (s *parseSession) commit(t *testing.T, fileName string, text string) {
	t.Helper()
	assert.NilError(t, s.utils.FS().WriteFile(fileName, text))
	response, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		Snapshot: s.snapshot,
		Changes: &CreateSnapshotParams{
			FileNotifications: &FileNotifications{Changed: []DocumentIdentifier{{FileName: fileName}}},
			SnapshotRequestChangesParams: SnapshotRequestChangesParams{
				EnsurePrograms: &EnsurePrograms{All: true},
			},
		},
	})
	assert.NilError(t, err)
	s.snapshot = response.Snapshot
}

func (s *parseSession) program() *compiler.Program {
	return s.project.Snapshot().ProjectCollection.ConfiguredProject(tspath.Path(parseSessionConfigFileName)).GetProgram()
}

func (s *parseSession) close() {
	s.session.Close()
	s.project.Close()
}

func decodeSourceFileResponse(t *testing.T, response any) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(response.(*SourceFileResponse).Data)
	assert.NilError(t, err)
	return data
}
