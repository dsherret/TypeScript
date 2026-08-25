package tsoptions_test

import (
	"slices"
	"testing"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tsoptions/tsoptionstest"
	"github.com/microsoft/typescript-go/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestWithAdditionalRootFiles checks that a command line with roots appended still
// reports everything the config parse produced. That is what lets a client name root
// files for a project without the compiler losing the config's own verdict on the
// options — the diagnostics, the source file the errors point into, and the raw object.
func TestWithAdditionalRootFiles(t *testing.T) {
	t.Parallel()

	base := parseConfigForTest(t, `{
		"compilerOptions": { "lib": ["not-a-lib"], "strict": true },
		"files": ["/p/a.ts"]
	}`, map[string]string{"/p/a.ts": "export const a = 1;"})
	derived := base.WithAdditionalRootFiles([]string{"/p/b.ts", "/p/c.ts"})

	assert.DeepEqual(t, derived.FileNames(), []string{"/p/a.ts", "/p/b.ts", "/p/c.ts"})
	// the base is left exactly as it was: every snapshot behind this one is still
	// reading it
	assert.DeepEqual(t, base.FileNames(), []string{"/p/a.ts"})

	assert.Equal(t, derived.ConfigName(), base.ConfigName())
	assert.Equal(t, derived.CompilerOptions(), base.CompilerOptions(), "the same options pointer, so canAddRootFiles compares equal")
	assert.DeepEqual(t, derived.LiteralFileNames(), base.LiteralFileNames())
	assert.Equal(t, derived.Raw, base.Raw)
	assert.Equal(t, derived.CompileOnSave, base.CompileOnSave)
	assert.DeepEqual(t, diagnosticCodes(derived.GetConfigFileParsingDiagnostics()), diagnosticCodes(base.GetConfigFileParsingDiagnostics()))
	// 6046: the argument for --lib must be one of the known libraries
	assert.Assert(t, slices.Contains(diagnosticCodes(derived.GetConfigFileParsingDiagnostics()), int32(6046)))
	// a fresh list, since CommonSourceDirectory appends to whichever of the two it is
	// asked of
	assert.Assert(t, len(base.Errors) > 0 && &derived.Errors[0] != &base.Errors[0], "the two share a backing array")

	// nothing appended, nothing derived
	assert.Equal(t, base.WithAdditionalRootFiles(nil), base)
}

// TestWithAdditionalRootFilesEmptyFilesList is the shape ts-morph's document registry
// writes: a config that names no files at all, with every root coming over the API.
func TestWithAdditionalRootFilesEmptyFilesList(t *testing.T) {
	t.Parallel()

	base := parseConfigForTest(t, `{ "compilerOptions": { "allowJs": true }, "files": [] }`, nil)
	// a present-but-empty `files` is what keeps the default `**/*` include off, so the
	// command line matches nothing until a root is named for it
	assert.Equal(t, len(base.FileNames()), 0)
	assert.Assert(t, !base.PossiblyMatchesFileName("/p/a.ts"))
	// 18002: the `files` list is empty, which is true of the config as written
	assert.Assert(t, slices.Contains(diagnosticCodes(base.GetConfigFileParsingDiagnostics()), int32(18002)))

	derived := base.WithAdditionalRootFiles([]string{"/p/a.ts", "/p/a.d.ts", "/p/a.js"})
	assert.DeepEqual(t, derived.FileNames(), []string{"/p/a.ts", "/p/a.d.ts", "/p/a.js"})
	assert.Equal(t, len(derived.LiteralFileNames()), 0)
	// and no longer true of this command line, whose root files are these three
	assert.Assert(t, !slices.Contains(diagnosticCodes(derived.GetConfigFileParsingDiagnostics()), int32(18002)))
}

func parseConfigForTest(t *testing.T, configText string, otherFiles map[string]string) *tsoptions.ParsedCommandLine {
	t.Helper()
	const configFileName = "/p/tsconfig.json"
	files := map[string]string{configFileName: configText}
	for name, text := range otherFiles {
		files[name] = text
	}
	host := tsoptionstest.NewVFSParseConfigHost(files, "/p", true /*useCaseSensitiveFileNames*/)
	sourceFile := parser.ParseSourceFile(ast.SourceFileParseOptions{
		FileName: configFileName,
		Path:     tspath.Path(configFileName),
	}, configText, core.ScriptKindJSON)
	return tsoptions.ParseJsonSourceFileConfigFileContent(
		&tsoptions.TsConfigSourceFile{SourceFile: sourceFile},
		host,
		"/p",
		nil, /*existingOptions*/
		nil, /*existingOptionsRaw*/
		configFileName,
		nil, /*resolutionStack*/
		nil, /*extraFileExtensions*/
		nil, /*extendedConfigCache*/
	)
}

func diagnosticCodes(diagnostics []*ast.Diagnostic) []int32 {
	codes := make([]int32, 0, len(diagnostics))
	for _, d := range diagnostics {
		codes = append(codes, d.Code())
	}
	return codes
}
