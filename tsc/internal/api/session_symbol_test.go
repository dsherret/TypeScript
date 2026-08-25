package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
	"gotest.tools/v3/assert"
)

// getSymbolAtLocation answers for the name of a declaration, so an anonymous one
// — an arrow function, an object literal, a call signature — has nothing to ask
// it with. getSymbolOfDeclaration asks the declaration itself, which is the only
// way to reach the symbol the binder gave it.

func TestGetSymbolOfDeclarationNamesAnAnonymousDeclaration(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	for kind, expected := range map[ast.Kind]string{
		ast.KindArrowFunction:           "__function",
		ast.KindFunctionExpression:      "__function",
		ast.KindObjectLiteralExpression: "__object",
		ast.KindTypeLiteral:             "__type",
		ast.KindMappedType:              "__type",
		ast.KindCallSignature:           "__call",
		ast.KindConstructSignature:      "__new",
		ast.KindIndexSignature:          "__index",
		ast.KindConstructor:             "__constructor",
		ast.KindExportAssignment:        "export=",
	} {
		symbol := symbolOfFirstNodeOfKind(t, session, snapshot, projectID, kind)
		assert.Assert(t, symbol != nil, "expected a symbol for %s", kind.String())
		assert.Equal(t, symbol.Name, expected, "for %s", kind.String())
	}
}

// A named declaration is answered the same way, so nothing has to decide which
// kinds may be asked.
func TestGetSymbolOfDeclarationNamesANamedDeclaration(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	symbol := symbolOfFirstNodeOfKind(t, session, snapshot, projectID, ast.KindClassDeclaration)
	assert.Assert(t, symbol != nil, "expected a symbol for a class declaration")
	assert.Equal(t, symbol.Name, "C")
}

// Nothing declares an array literal, and a caller that asks about one must get
// no symbol rather than a symbol from somewhere nearby.
func TestGetSymbolOfDeclarationIsNilForANodeThatDeclaresNothing(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	for _, kind := range []ast.Kind{ast.KindArrayLiteralExpression, ast.KindCallExpression, ast.KindBlock} {
		symbol := symbolOfFirstNodeOfKind(t, session, snapshot, projectID, kind)
		assert.Assert(t, symbol == nil, "expected no symbol for %s, got %v", kind.String(), symbol)
	}
}

// A module symbol has no name in the text: it reads as the specifier that would
// import it, which is a question about where it is being asked from. Two asking
// files, two answers for the one symbol.
func TestSymbolToStringNamesAModuleByItsSpecifier(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	moduleSymbol := symbolAtStartOfFile(t, session, snapshot, projectID, symbolModPath)
	assert.Assert(t, moduleSymbol != nil, "expected the module symbol of mod.ts")

	assert.Equal(t, symbolToStringAtStartOfFile(t, session, snapshot, projectID, moduleSymbol.Id, symbolMainPath), `"../mod"`)
	assert.Equal(t, symbolToStringAtStartOfFile(t, session, snapshot, projectID, moduleSymbol.Id, symbolRootPath), `"./mod"`)
}

// Without a location there is no file to write a specifier for, so the symbol
// falls back to naming itself. This pins that an omitted location is not read as
// some default file.
func TestSymbolToStringWithoutALocationNamesTheModuleItself(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	moduleSymbol := symbolAtStartOfFile(t, session, snapshot, projectID, symbolModPath)
	assert.Assert(t, moduleSymbol != nil, "expected the module symbol of mod.ts")

	result, err := session.handleSymbolToString(context.Background(), &SymbolToStringParams{
		Snapshot: snapshot,
		Project:  projectID,
		Symbol:   moduleSymbol.Id,
	})
	assert.NilError(t, err)
	assert.Equal(t, result, fmt.Sprintf("%q", symbolModPath[:len(symbolModPath)-len(".ts")]))
}

// A symbol that is not a module is named by its own name, qualified by what
// declares it.
func TestSymbolToStringNamesAMemberByItsContainer(t *testing.T) {
	t.Parallel()
	session, snapshot, projectID := setupSymbolProject(t)

	classSymbol := symbolOfFirstNodeOfKind(t, session, snapshot, projectID, ast.KindMethodDeclaration)
	assert.Assert(t, classSymbol != nil, "expected the method's symbol")

	assert.Equal(t, symbolToStringAtStartOfFile(t, session, snapshot, projectID, classSymbol.Id, symbolMainPath), "C.m")
}

const (
	symbolMainPath = "/home/projects/p/dir/main.ts"
	symbolModPath  = "/home/projects/p/mod.ts"
	symbolRootPath = "/home/projects/p/root.ts"

	// every anonymous declaration the checker gives an internal name to, in one file
	symbolMain = "const arrow = (a: number) => a;\n" +
		"const anon = function (a: number) { return a; };\n" +
		"const obj = { p: 1 };\n" +
		"const arr = [1, 2, 3];\n" +
		"type Lit = { q: number };\n" +
		"type Mapped = { [K in \"a\"]: number };\n" +
		"interface WithSigs { (a: string): void; new (b: number): WithSigs; [key: string]: any; }\n" +
		"class C { constructor(a: number) {} m() {} }\n" +
		"arrow(1);\n" +
		"export = C;\n"
)

// symbolOfFirstNodeOfKind is what getSymbolOfDeclaration answers for the first
// node of `kind` in main.ts, in source order.
func symbolOfFirstNodeOfKind(t *testing.T, session *Session, snapshot SnapshotID, projectID ProjectID, kind ast.Kind) *SymbolResponse {
	t.Helper()
	symbol, err := session.handleGetSymbolOfDeclaration(context.Background(), &GetSymbolOfDeclarationParams{
		Snapshot:    snapshot,
		Project:     projectID,
		Declaration: nodeHandleOfFirstNodeOfKind(t, session, snapshot, projectID, symbolMainPath, kind),
	})
	assert.NilError(t, err)
	return symbol
}

// symbolAtStartOfFile is the symbol at offset zero of a file, which for a module
// is the file's own module symbol.
func symbolAtStartOfFile(t *testing.T, session *Session, snapshot SnapshotID, projectID ProjectID, fileName string) *SymbolResponse {
	t.Helper()
	symbol, err := session.handleGetSymbolAtLocation(context.Background(), &GetSymbolAtLocationParams{
		Snapshot: snapshot,
		Project:  projectID,
		Location: nodeHandleOfFirstNodeOfKind(t, session, snapshot, projectID, fileName, ast.KindSourceFile),
	})
	assert.NilError(t, err)
	return symbol
}

func symbolToStringAtStartOfFile(t *testing.T, session *Session, snapshot SnapshotID, projectID ProjectID, symbol SymbolID, fileName string) string {
	t.Helper()
	result, err := session.handleSymbolToString(context.Background(), &SymbolToStringParams{
		Snapshot: snapshot,
		Project:  projectID,
		Symbol:   symbol,
		Location: nodeHandleOfFirstNodeOfKind(t, session, snapshot, projectID, fileName, ast.KindSourceFile),
	})
	assert.NilError(t, err)
	text, ok := result.(string)
	assert.Assert(t, ok, "expected a string, got %T", result)
	return text
}

// nodeHandleOfFirstNodeOfKind walks the file the way a client's own traversal
// would, so the handles under test are the ones a client would send.
func nodeHandleOfFirstNodeOfKind(
	t *testing.T,
	session *Session,
	snapshot SnapshotID,
	projectID ProjectID,
	fileName string,
	kind ast.Kind,
) NodeHandle {
	t.Helper()
	setup, err := session.setupChecker(context.Background(), snapshot, projectID)
	assert.NilError(t, err)
	defer setup.done()

	sourceFile := setup.program.GetSourceFile(fileName)
	assert.Assert(t, sourceFile != nil, "%s is not in the program", fileName)

	found := findFirstNodeOfKind(sourceFile.AsNode(), kind)
	assert.Assert(t, found != nil, "%s has no %s", fileName, kind.String())
	return setup.sd.nodeHandleFrom(found)
}

func findFirstNodeOfKind(node *ast.Node, kind ast.Kind) *ast.Node {
	if node.Kind == kind {
		return node
	}
	var found *ast.Node
	node.ForEachChild(func(child *ast.Node) bool {
		found = findFirstNodeOfKind(child, kind)
		return found != nil
	})
	return found
}

func setupSymbolProject(t *testing.T) (*Session, SnapshotID, ProjectID) {
	t.Helper()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]any{
		"/home/projects/p/tsconfig.json": `{ "compilerOptions": { "strict": true } }`,
		symbolMainPath:                   symbolMain,
		symbolModPath:                    "export const modValue = 1;\n",
		symbolRootPath:                   "import { modValue } from \"./mod\";\nmodValue;\n",
	}

	projectSession, _ := projecttestutil.Setup(files)
	t.Cleanup(projectSession.Close)
	session := NewSession(projectSession, nil)
	t.Cleanup(session.Close)

	ctx := context.Background()
	snapshotResp, err := session.handleUpdateSnapshot(ctx, &UpdateSnapshotParams{
		OpenFiles: []DocumentIdentifier{{FileName: symbolMainPath}, {FileName: symbolRootPath}},
	})
	assert.NilError(t, err)

	proj, err := session.handleGetDefaultProjectForFile(ctx, &GetDefaultProjectForFileParams{
		Snapshot: snapshotResp.Snapshot,
		File:     DocumentIdentifier{FileName: symbolMainPath},
	})
	assert.NilError(t, err)
	assert.Assert(t, proj != nil, "file should resolve to a default project")

	return session, snapshotResp.Snapshot, proj.Id
}
