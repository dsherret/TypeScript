package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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
	"github.com/zeebo/xxh3"
	"gotest.tools/v3/assert"
)

// TestParseSourceFileNodeIdentityMatchesTheProgram is the premise the parse-only endpoint
// rests on: a node handle is an index into the table encoder.BuildNodeIndexTable walks,
// and that index is a pure function of the AST shape. If a standalone parse numbered its
// nodes differently from the program's own parse of the same text, every handle minted
// from a parse-only tree would resolve to the wrong node — silently, since a handle
// carries no shape the server checks.
//
// Parse options are the one thing that could differ between the two parses, since the
// endpoint takes them from a snapshot the caller names and may have none. They cannot
// move a node: ast.SetExternalModuleIndicator runs after the parse and only points
// SourceFile.ExternalModuleIndicator at a node that is already there.
func TestParseSourceFileNodeIdentityMatchesTheProgram(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/id.ts"
	text := `import { b } from "./b";
export class C {
  m(x: number): string { return x.toString() + b; }
}
export const d = { e: 1, f: [2, 3] };
export default function g<T>(t: T): T { return t; }
`
	s := newParseSession(t, map[string]string{
		fileName:  text,
		"/p/b.ts": `export const b = "b";`,
	}, nil)
	defer s.close()

	fromProgram := s.program().GetSourceFileByPath(tspath.Path(fileName))
	assert.Assert(t, fromProgram != nil)
	programTable := encoder.GetNodeIndexTable(fromProgram)
	programBytes, _, err := encoder.EncodeSourceFile(fromProgram)
	assert.NilError(t, err)

	for _, opts := range []ast.ExternalModuleIndicatorOptions{
		{},
		{JSX: true},
		{Force: true},
		{JSX: true, Force: true},
	} {
		standalone := parser.ParseSourceFile(
			ast.SourceFileParseOptions{FileName: fileName, Path: tspath.Path(fileName), ExternalModuleIndicatorOptions: opts},
			text,
			core.GetScriptKindFromFileName(fileName),
		)
		standalone.Hash = xxh3.HashString128(text)
		assertSameNodeIndexTable(t, programTable, encoder.GetNodeIndexTable(standalone))

		standaloneBytes, _, err := encoder.EncodeSourceFile(standalone)
		assert.NilError(t, err)
		assert.Equal(t, len(standaloneBytes), len(programBytes))
		// everything but the parse options byte, which is the header field the four
		// combinations above set and the only thing they change
		assert.DeepEqual(t, standaloneBytes[:encoder.HeaderOffsetParseOptions], programBytes[:encoder.HeaderOffsetParseOptions])
		assert.DeepEqual(t, standaloneBytes[encoder.HeaderOffsetParseOptions+4:], programBytes[encoder.HeaderOffsetParseOptions+4:])
	}
}

// TestParseSourceFileMatchesGetSourceFile drives the endpoint the way a client does and
// checks that what comes back is what getSourceFile returns for the same text — which is
// what makes a parse-only tree interchangeable with the program's own, both for node
// handles and for the client's source file cache.
//
// The comparison is against a tree parsed here, independently, and not only against
// getSourceFile: the endpoint's parse goes through the program's own parse cache, so for
// text the program already holds the two responses encode the *same object* and comparing
// them could not fail. An independent parse is what gives the comparison teeth, and the
// endpoint is checked against both.
//
// The one thing that legitimately differs is the node flags the *binder* sets, and the
// error flag a node aggregates on demand: those are written onto the program's tree after
// it is parsed, and a tree nothing has bound does not carry them — see
// binderInitializedNodeFlags. They are not part of a node's identity and nothing in
// ts-morph reads them; which of the two a client saw was already a question of whether
// anything had bound the file before it first asked for it.
func TestParseSourceFileMatchesGetSourceFile(t *testing.T) {
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
		{"empty", "/p/a.ts", ""},
		{"jsdoc", "/p/a.js", "/** @param {number} x */\nexport function f(x) { return x; }\n"},
		{"syntax error", "/p/a.ts", "export class {{{\n"},
		{"non-ascii", "/p/a.ts", "export const \U0001F600 = \"é中文\";\n"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			s := newParseSession(t, map[string]string{testCase.file: testCase.text}, map[string]any{"allowJs": true})
			defer s.close()

			viaProgram, err := s.session.handleGetSourceFile(context.Background(), &GetSourceFileParams{
				Snapshot: s.snapshot,
				Project:  s.projectID,
				File:     DocumentIdentifier{FileName: testCase.file},
			})
			assert.NilError(t, err)

			viaParse, err := s.session.handleParseSourceFile(context.Background(), &ParseSourceFileParams{
				File:     DocumentIdentifier{FileName: testCase.file},
				Text:     testCase.text,
				Snapshot: s.snapshot,
				Project:  s.projectID,
			})
			assert.NilError(t, err)

			fromProgram := s.program().GetSourceFileByPath(tspath.Path(testCase.file))
			standalone := parser.ParseSourceFile(fromProgram.ParseOptions(), testCase.text, fromProgram.ScriptKind)
			standalone.Hash = xxh3.HashString128(testCase.text)
			standaloneBytes, _, err := encoder.EncodeSourceFile(standalone)
			assert.NilError(t, err)

			assertSameEncodingIgnoringBinderFlags(t, standaloneBytes, decodeSourceFileResponse(t, viaProgram))
			assertSameEncodingIgnoringBinderFlags(t, decodeSourceFileResponse(t, viaParse), standaloneBytes)
		})
	}
}

// TestParseSourceFileIsTheTreeTheProgramTakes is the property the endpoint's cost rests
// on: the text is parsed once, not once here and again when a program is built over it.
//
// It is checked by object identity rather than by a counter, because identity is the
// stronger statement — the program did not merely produce an equal tree, it took this one,
// which is only possible if it found it in the cache instead of parsing. The cases are the
// ones that decide whether the tree is found: every script kind, module detection the file
// itself does not settle, text the file system does not have, and a path the program does
// not hold at all.
func TestParseSourceFileIsTheTreeTheProgramTakes(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.ts"
	const edited = "export const a = 1;\nexport class C {}\n"

	// one per script kind, since the script kind is part of the key and is the one part of
	// it this side works out for itself rather than reading off the program
	t.Run("the program takes the offered tree", func(t *testing.T) {
		t.Parallel()
		for _, testCase := range []struct{ file, text, edited string }{
			{"/p/f.ts", "export const a = 1;\n", "export const a = 1;\nexport class C {}\n"},
			{"/p/f.tsx", "export const A = () => <div>hi</div>;\n", "export const A = () => <div>bye</div>;\n"},
			{"/p/f.mts", "export const a = 1;\n", "export const a = 2;\n"},
			{"/p/f.cts", "export const a = 1;\n", "export const a = 2;\n"},
			{"/p/f.js", "export const a = 1;\n", "export const a = 2;\n"},
			{"/p/f.mjs", "export const a = 1;\n", "export const a = 2;\n"},
			{"/p/f.cjs", "const a = 1;\n", "const a = 2;\n"},
			{"/p/f.jsx", "export const A = () => <div>hi</div>;\n", "export const A = () => <div>bye</div>;\n"},
			{"/p/f.d.ts", "export declare const a: number;\n", "export declare const a: string;\n"},
			{"/p/f.json", `{"a":1}`, `{"a":2}`},
		} {
			t.Run(testCase.file, func(t *testing.T) {
				t.Parallel()
				s := newParseSession(t, map[string]string{testCase.file: testCase.text},
					map[string]any{"allowJs": true, "resolveJsonModule": true, "jsx": "react"})
				defer s.close()

				offered := s.offer(t, testCase.file, testCase.edited)
				s.commit(t, testCase.file, testCase.edited)
				assert.Equal(t, s.program().GetSourceFileByPath(tspath.Path(testCase.file)), offered)
			})
		}
	})

	// module detection from a package.json is the one parse option the file's own text and
	// name do not settle, and it is read off the program the client named
	t.Run("the program takes it under a package scope it cannot see", func(t *testing.T) {
		t.Parallel()
		const scopedFileName = "/p/scoped.js"
		s := newParseSession(t, map[string]string{
			scopedFileName:    "const a = 1;\n",
			"/p/package.json": `{"type":"module"}`,
		}, map[string]any{"allowJs": true, "module": "nodenext", "moduleResolution": "nodenext"})
		defer s.close()

		const scopedEdited = "const a = 2;\n"
		offered := s.offer(t, scopedFileName, scopedEdited)
		assert.Assert(t, offered.ParseOptions().ExternalModuleIndicatorOptions.Force)
		s.commit(t, scopedFileName, scopedEdited)
		assert.Equal(t, s.program().GetSourceFileByPath(tspath.Path(scopedFileName)), offered)
	})

	t.Run("text the file system does not have is not taken", func(t *testing.T) {
		t.Parallel()
		s := newParseSession(t, map[string]string{fileName: "export const a = 1;\n"}, nil)
		defer s.close()

		offered := s.offer(t, fileName, edited)
		s.commit(t, fileName, "export const a = 2;\n")
		fromProgram := s.program().GetSourceFileByPath(tspath.Path(fileName))
		assert.Assert(t, fromProgram != offered)
		assert.Equal(t, fromProgram.Text(), "export const a = 2;\n")
	})

	t.Run("a path no program holds is dropped", func(t *testing.T) {
		t.Parallel()
		s := newParseSession(t, map[string]string{fileName: "export const a = 1;\n"}, nil)
		defer s.close()

		const strayFileName = "/p/stray.ts"
		s.offer(t, strayFileName, edited)
		s.commit(t, fileName, edited)
		assert.Assert(t, s.program().GetSourceFileByPath(tspath.Path(strayFileName)) == nil)
	})
}

// TestParseSourceFileWithoutASnapshot checks the endpoint answers with no snapshot named
// at all — which is what a client that has never opened a project sends — and with one
// that has gone, which is the same case.
func TestParseSourceFileWithoutASnapshot(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newParseSession(t, map[string]string{"/p/a.ts": "export const a = 1;\n"}, nil)
	defer s.close()

	const text = "export const b = 2;\n"
	withNone, err := s.session.handleParseSourceFile(context.Background(), &ParseSourceFileParams{
		File: DocumentIdentifier{FileName: "/p/never-seen.ts"},
		Text: text,
	})
	assert.NilError(t, err)
	assert.Assert(t, withNone != nil)

	withStale, err := s.session.handleParseSourceFile(context.Background(), &ParseSourceFileParams{
		File:     DocumentIdentifier{FileName: "/p/never-seen.ts"},
		Text:     text,
		Snapshot: 99999,
		Project:  "no-such-project",
	})
	assert.NilError(t, err)
	assert.DeepEqual(t, withStale, withNone)
}

// TestParseSourceFileHashMatchesTheProgram checks the header hash, which is what the
// client's source file cache keys on. The parser leaves it zero, so the endpoint fills it
// in; if it filled in a different hash from the one a file handle carries, an entry the
// cache put there would never be matched and the parse would be paid twice.
func TestParseSourceFileHashMatchesTheProgram(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.ts"
	const text = "export const a = 1;\n"
	s := newParseSession(t, map[string]string{fileName: text}, nil)
	defer s.close()

	response, err := s.session.handleParseSourceFile(context.Background(), &ParseSourceFileParams{
		File:     DocumentIdentifier{FileName: fileName},
		Text:     text,
		Snapshot: s.snapshot,
		Project:  s.projectID,
	})
	assert.NilError(t, err)
	data := decodeSourceFileResponse(t, response)
	assert.Equal(t, binary.LittleEndian.Uint64(data[encoder.HeaderOffsetHashLo0:]), s.program().GetSourceFileByPath(tspath.Path(fileName)).Hash.Lo)
	assert.Equal(t, binary.LittleEndian.Uint64(data[encoder.HeaderOffsetHashHi0:]), s.program().GetSourceFileByPath(tspath.Path(fileName)).Hash.Hi)
}

// TestParseSourceFileTakesParseOptionsFromTheProgram checks the one thing read off the
// snapshot: module detection under a `"type": "module"` package scope, where the file's
// own metadata decides whether it is a module. Getting this wrong cannot move a node —
// it reports the wrong module-ness for the file.
func TestParseSourceFileTakesParseOptionsFromTheProgram(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.js"
	s := newParseSession(t, map[string]string{
		fileName:          "const a = 1;\n",
		"/p/package.json": `{"type":"module"}`,
	}, map[string]any{"allowJs": true, "module": "nodenext", "moduleResolution": "nodenext"})
	defer s.close()

	fromProgram := s.program().GetSourceFileByPath(tspath.Path(fileName))
	assert.Assert(t, fromProgram != nil)
	assert.Assert(t, fromProgram.ParseOptions().ExternalModuleIndicatorOptions.Force)

	params := &ParseSourceFileParams{Snapshot: s.snapshot, Project: s.projectID}
	assert.Equal(t,
		s.session.externalModuleIndicatorOptionsFor(params, fileName, tspath.Path(fileName)),
		fromProgram.ParseOptions().ExternalModuleIndicatorOptions)

	// and for a file the program does not hold yet, which is what a create sends
	const newFileName = "/p/b.js"
	assert.Assert(t, s.session.externalModuleIndicatorOptionsFor(params, newFileName, tspath.Path(newFileName)).Force)
}

// TestParseSourceFileWorksOutTheMetadataOfAFileTheProgramLacks is the same question with
// nothing else able to answer it. Under the default `moduleDetection` every non-declaration
// file is forced to be a module whatever its package scope says, so the previous test would
// pass reading no metadata at all; under `auto` the package scope is the whole answer, and
// the program has none recorded for a file it does not hold.
//
// Reading the recorded metadata here rather than working it out says "no package scope",
// which is a wrong answer rather than a missing one: the created file is parsed as a script
// under a `"type": "module"` scope, and the tree is filed where the program will not look
// for it — so the text is parsed twice as well as described wrongly.
func TestParseSourceFileWorksOutTheMetadataOfAFileTheProgramLacks(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const fileName = "/p/a.js"
	s := newParseSession(t, map[string]string{
		fileName:          "const a = 1;\n",
		"/p/package.json": `{"type":"module"}`,
	}, map[string]any{
		"allowJs": true, "module": "nodenext", "moduleResolution": "nodenext", "moduleDetection": "auto",
	})
	defer s.close()

	// the file the program does hold is forced, and by its package scope alone
	fromProgram := s.program().GetSourceFileByPath(tspath.Path(fileName))
	assert.Assert(t, fromProgram != nil)
	assert.Assert(t, fromProgram.ParseOptions().ExternalModuleIndicatorOptions.Force)

	const newFileName = "/p/b.js"
	params := &ParseSourceFileParams{Snapshot: s.snapshot, Project: s.projectID}
	assert.Assert(t, s.session.externalModuleIndicatorOptionsFor(params, newFileName, tspath.Path(newFileName)).Force)
}

// binderInitializedNodeFlags are the ast.NodeFlags a node does not carry when it is
// parsed: the binder writes them onto the program's tree, and
// NodeFlagsThisNodeOrAnySubNodesHasError is aggregated the first time it is asked for.
const binderInitializedNodeFlags = uint32(ast.NodeFlagsExportContext |
	ast.NodeFlagsContainsThis |
	ast.NodeFlagsHasImplicitReturn |
	ast.NodeFlagsHasExplicitReturn |
	ast.NodeFlagsThisNodeOrAnySubNodesHasError |
	ast.NodeFlagsHasAsyncFunctions |
	ast.NodeFlagsUnreachable)

func assertSameEncodingIgnoringBinderFlags(t *testing.T, actual []byte, expected []byte) {
	t.Helper()
	assert.Equal(t, len(actual), len(expected))
	nodesOffset := int(binary.LittleEndian.Uint32(expected[encoder.HeaderOffsetNodes:]))
	assert.DeepEqual(t, actual[:nodesOffset], expected[:nodesOffset])
	for offset := nodesOffset; offset < len(expected); offset += nodeSizeInBytes {
		assert.DeepEqual(t, actual[offset:offset+nodeFlagsOffset], expected[offset:offset+nodeFlagsOffset])
		actualFlags := binary.LittleEndian.Uint32(actual[offset+nodeFlagsOffset:])
		expectedFlags := binary.LittleEndian.Uint32(expected[offset+nodeFlagsOffset:])
		assert.Equal(t,
			actualFlags&^binderInitializedNodeFlags,
			expectedFlags&^binderInitializedNodeFlags,
			"node %d flags", (offset-nodesOffset)/nodeSizeInBytes)
	}
}

const (
	nodeSizeInBytes = 28
	nodeFlagsOffset = 24
)

func decodeSourceFileResponse(t *testing.T, response any) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(response.(*SourceFileResponse).Data)
	assert.NilError(t, err)
	return data
}

func assertSameNodeIndexTable(t *testing.T, expected *encoder.NodeIndexTable, actual *encoder.NodeIndexTable) {
	t.Helper()
	assert.Assert(t, expected != nil)
	assert.Assert(t, actual != nil)
	assert.Equal(t, len(actual.Nodes), len(expected.Nodes))
	for i := range expected.Nodes {
		expectedNode, actualNode := expected.Nodes[i], actual.Nodes[i]
		if expectedNode == nil || actualNode == nil {
			assert.Equal(t, actualNode == nil, expectedNode == nil, "node %d", i)
			continue
		}
		assert.Equal(t, actualNode.Kind, expectedNode.Kind, "node %d", i)
		assert.Equal(t, actualNode.Pos(), expectedNode.Pos(), "node %d", i)
		assert.Equal(t, actualNode.End(), expectedNode.End(), "node %d", i)
	}
}

const parseSessionConfigFileName = "/p/tsconfig.json"

type parseSession struct {
	session   *Session
	project   *project.Session
	utils     *projecttestutil.SessionUtils
	snapshot  SnapshotID
	projectID ProjectID
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
	s := &parseSession{session: NewSession(projectSession, nil), project: projectSession, utils: utils}
	response, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		OpenProjects: []DocumentIdentifier{{FileName: parseSessionConfigFileName}},
	})
	assert.NilError(t, err)
	assert.Assert(t, len(response.Projects) > 0)
	s.snapshot = response.Snapshot
	s.projectID = response.Projects[0].Id
	return s
}

// offer parses text the way the endpoint does, and hands back the tree so a caller can
// check what became of it. What handleParseSourceFile does with the tree is encode it.
func (s *parseSession) offer(t *testing.T, fileName string, text string) *ast.SourceFile {
	t.Helper()
	params := &ParseSourceFileParams{Snapshot: s.snapshot, Project: s.projectID}
	path := tspath.Path(fileName)
	return s.project.ParseSourceFile(ast.SourceFileParseOptions{
		FileName:                       fileName,
		Path:                           path,
		ExternalModuleIndicatorOptions: s.session.externalModuleIndicatorOptionsFor(params, fileName, path),
	}, text)
}

// commit writes text where the compiler reads it and builds the snapshot that picks it up,
// which is the flush a client's held-back edit reaches the compiler through.
func (s *parseSession) commit(t *testing.T, fileName string, text string) {
	t.Helper()
	assert.NilError(t, s.utils.FS().WriteFile(fileName, text))
	response, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		FileChanges: &APIFileChanges{Changed: []DocumentIdentifier{{FileName: fileName}}},
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
