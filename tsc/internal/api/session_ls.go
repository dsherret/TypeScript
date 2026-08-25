package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ls"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsconv"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsutil"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/project"
)

// Language service handlers.
//
// The editing operations below (formatting, organize imports) are implemented
// in internal/ls for the LSP server. They are exposed here so API clients —
// which work in character offsets rather than LSP positions, and drive edits
// programmatically rather than through an editor — can use them too. Results are
// converted to the API's offset-based TextEdit.

// handleFormatDocument returns the edits that format an entire file.
func (s *Session) handleFormatDocument(ctx context.Context, params *FormatDocumentParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	edits := setup.langSvc.FormatDocumentWithSettings(ctx, setup.documentURI, toFormatCodeSettings(setup.langSvc.FormatOptions(), params.Options))
	return toAPITextEdits(setup.sourceFile, edits), nil
}

// handleFormatDocumentRange returns the edits that format a span of a file.
func (s *Session) handleFormatDocumentRange(ctx context.Context, params *FormatDocumentRangeParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	converters := setup.snapshot.Converters()
	positionMap := setup.sourceFile.GetPositionMap()
	lspRange := rangeToLSP(converters, setup.sourceFile, core.NewTextRange(
		positionMap.UTF16ToUTF8(params.Pos),
		positionMap.UTF16ToUTF8(params.End),
	))

	edits := setup.langSvc.FormatDocumentRangeWithSettings(ctx, setup.documentURI, toFormatCodeSettings(setup.langSvc.FormatOptions(), params.Options), lspRange)
	return toAPITextEdits(setup.sourceFile, edits), nil
}

// handleOrganizeImports returns the edits that sort, combine, and/or remove
// unused imports in a file, according to the requested mode.
func (s *Session) handleOrganizeImports(ctx context.Context, params *OrganizeImportsParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	kind := lsproto.CodeActionKindSourceOrganizeImports
	switch params.Mode {
	case "", OrganizeImportsModeAll:
		kind = lsproto.CodeActionKindSourceOrganizeImports
	case OrganizeImportsModeSortAndCombine:
		kind = lsproto.CodeActionKindSourceSortImports
	case OrganizeImportsModeRemoveUnused:
		kind = lsproto.CodeActionKindSourceRemoveUnusedImports
	default:
		return nil, fmt.Errorf("%w: unknown organize imports mode %q", ErrClientError, params.Mode)
	}

	editsByFile := setup.langSvc.OrganizeImports(ctx, setup.sourceFile, setup.program, kind)
	return toAPITextEdits(setup.sourceFile, editsByFile[setup.sourceFile.FileName()]), nil
}

// handleRename returns the edits that rename the symbol at a position, grouped
// by file. An empty result means the element cannot be renamed.
func (s *Session) handleRename(ctx context.Context, params *RenameParams) ([]*FileTextEdits, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	if params.UseAliasesForRename != nil {
		setup.langSvc.SetUseAliasesForRename(core.IfElse(*params.UseAliasesForRename, core.TSTrue, core.TSFalse))
	}

	// A nil orchestrator selects the single-project path: renames are resolved
	// against this project's program only, which is the API's model. Zero
	// RenameOptions leave off the editor's eligibility checks, matching
	// findRenameLocations rather than getRenameInfo: this caller owns every file
	// in its program, including anything it chose to place under node_modules.
	response, err := setup.langSvc.ProvideRename(ctx, &lsproto.RenameParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Position:     setup.toLSPPosition(params.Position),
		NewName:      params.NewName,
	}, nil, ls.RenameOptions{})
	if err != nil {
		return nil, err
	}
	if response.WorkspaceEdit == nil {
		return []*FileTextEdits{}, nil
	}
	// DocumentChanges carries versioned edits plus create/rename/delete-file
	// operations, none of which this API models. It is only populated when the
	// client advertises the capability, which it deliberately does not (see
	// InProcessServerOptions) — fail loudly if that ever changes rather than
	// returning a truncated edit set.
	if response.WorkspaceEdit.DocumentChanges != nil {
		return nil, fmt.Errorf("%w: rename returned unsupported document changes", ErrClientError)
	}
	if response.WorkspaceEdit.Changes == nil {
		return []*FileTextEdits{}, nil
	}

	result := make([]*FileTextEdits, 0, len(*response.WorkspaceEdit.Changes))
	for uri, edits := range *response.WorkspaceEdit.Changes {
		fileName := uri.FileName()
		sourceFile := setup.program.GetSourceFile(fileName)
		if sourceFile == nil {
			// Dropping this file's edits would silently return a partial rename,
			// which corrupts the source it is applied to.
			return nil, fmt.Errorf("%w: rename touches a file that is not in the program: %s", ErrClientError, fileName)
		}
		result = append(result, &FileTextEdits{
			FileName: fileName,
			Edits:    toAPITextEdits(sourceFile, edits),
		})
	}
	// The wire order of a map is unspecified; sort so results are deterministic.
	slices.SortFunc(result, func(a, b *FileTextEdits) int {
		return strings.Compare(a.FileName, b.FileName)
	})
	return result, nil
}

// handleGetDefinition returns the locations that define the symbol at a position.
func (s *Session) handleGetDefinition(ctx context.Context, params *FilePositionParams) ([]*FileSpan, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	response, err := setup.langSvc.ProvideDefinition(ctx, setup.documentURI, setup.toLSPPosition(params.Position))
	if err != nil {
		return nil, err
	}
	return setup.toAPIFileSpans(response), nil
}

// handleGetImplementations returns the locations that implement the symbol at a
// position.
func (s *Session) handleGetImplementations(ctx context.Context, params *FilePositionParams) ([]*FileSpan, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	// As with rename, a nil orchestrator selects the single-project path.
	response, err := setup.langSvc.ProvideImplementations(ctx, &lsproto.ImplementationParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Position:     setup.toLSPPosition(params.Position),
	}, nil)
	if err != nil {
		return nil, err
	}
	return setup.toAPIFileSpans(response), nil
}

// handleGetCodeFixes returns the quick fixes available for a span of a file.
//
// The LSP model derives fixes from diagnostics the client already holds, while
// API clients ask by error code, so the diagnostics are computed here, filtered
// to the requested span (and error codes, when given), and handed back as the
// code action context.
func (s *Session) handleGetCodeFixes(ctx context.Context, params *GetCodeFixesParams) ([]*CodeFixAction, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, true)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	if err := applyQuotePreference(setup.langSvc, params.QuotePreference); err != nil {
		return nil, err
	}

	diagnosticsResponse, err := setup.langSvc.ProvideDiagnostics(ctx, setup.documentURI)
	if err != nil {
		return nil, err
	}
	if diagnosticsResponse.FullDocumentDiagnosticReport == nil {
		return []*CodeFixAction{}, nil
	}

	start, end := params.Pos, params.End
	if end < start {
		start, end = end, start
	}
	converters := setup.snapshot.Converters()
	positionMap := setup.sourceFile.GetPositionMap()

	var relevant []*lsproto.Diagnostic
	for _, diagnostic := range diagnosticsResponse.FullDocumentDiagnosticReport.Items {
		if diagnostic == nil {
			continue
		}
		if len(params.ErrorCodes) > 0 {
			if diagnostic.Code == nil || diagnostic.Code.Integer == nil ||
				!slices.Contains(params.ErrorCodes, int(*diagnostic.Code.Integer)) {
				continue
			}
		}
		diagStart := positionMap.UTF8ToUTF16(int(posFromLSP(converters, setup.sourceFile, diagnostic.Range.Start)))
		diagEnd := positionMap.UTF8ToUTF16(int(posFromLSP(converters, setup.sourceFile, diagnostic.Range.End)))
		if diagEnd < start || diagStart > end {
			continue
		}
		relevant = append(relevant, diagnostic)
	}
	if len(relevant) == 0 {
		return []*CodeFixAction{}, nil
	}

	only := []lsproto.CodeActionKind{lsproto.CodeActionKindQuickFix}
	response, err := setup.langSvc.ProvideCodeActions(ctx, &lsproto.CodeActionParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Range: rangeToLSP(converters, setup.sourceFile, core.NewTextRange(
			positionMap.UTF16ToUTF8(start),
			positionMap.UTF16ToUTF8(end),
		)),
		Context: &lsproto.CodeActionContext{Diagnostics: relevant, Only: &only},
	})
	if err != nil {
		return nil, err
	}
	if response.CommandOrCodeActionArray == nil {
		return []*CodeFixAction{}, nil
	}

	result := make([]*CodeFixAction, 0, len(*response.CommandOrCodeActionArray))
	for _, entry := range *response.CommandOrCodeActionArray {
		action := entry.CodeAction
		if action == nil || action.Edit == nil || action.Edit.Changes == nil {
			continue
		}
		if action.Edit.DocumentChanges != nil {
			return nil, fmt.Errorf("%w: code fix %q returned unsupported document changes", ErrClientError, action.Title)
		}
		fix := &CodeFixAction{Description: action.Title}
		incomplete := false
		for uri, edits := range *action.Edit.Changes {
			fileName := uri.FileName()
			sourceFile := setup.program.GetSourceFile(fileName)
			if sourceFile == nil {
				// A fix is a unit: applying only the part that lands in the
				// program would leave the code half-fixed. Drop the whole fix.
				incomplete = true
				break
			}
			fix.Changes = append(fix.Changes, &FileTextEdits{
				FileName: fileName,
				Edits:    toAPITextEdits(sourceFile, edits),
			})
		}
		if incomplete {
			continue
		}
		slices.SortFunc(fix.Changes, func(a, b *FileTextEdits) int {
			return strings.Compare(a.FileName, b.FileName)
		})
		if len(fix.Changes) > 0 {
			result = append(result, fix)
		}
	}
	return result, nil
}

// handleGetCombinedCodeFix returns the edits that apply one fix id across a
// whole file, i.e. the "fix all" form of a quick fix.
//
// Unlike handleGetCodeFixes this does not go through the LSP code action shape:
// the provider's combined result is already a plain edit list, and routing it
// through lsproto.CodeAction would only lose the description.
func (s *Session) handleGetCombinedCodeFix(ctx context.Context, params *GetCombinedCodeFixParams) (*CombinedCodeActions, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, true)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	if err := applyQuotePreference(setup.langSvc, params.QuotePreference); err != nil {
		return nil, err
	}

	var formatOptions *lsutil.FormatCodeSettings
	if params.Options != nil {
		settings := toFormatCodeSettings(setup.langSvc.FormatOptions(), params.Options)
		formatOptions = &settings
	}

	combined, known, err := setup.langSvc.GetCombinedCodeFix(ctx, setup.program, setup.sourceFile, params.FixId, formatOptions)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, fmt.Errorf("%w: no code fix provider handles fix id %q", ErrClientError, params.FixId)
	}

	// A provider that owns the fix id but finds nothing to fix returns nil, which
	// is an empty change set rather than an error.
	result := &CombinedCodeActions{Changes: []*FileTextEdits{}}
	if combined != nil {
		result.Description = combined.Description
	}
	if combined != nil && len(combined.Changes) > 0 {
		result.Changes = append(result.Changes, &FileTextEdits{
			FileName: setup.sourceFile.FileName(),
			Edits:    toAPITextEdits(setup.sourceFile, combined.Changes),
		})
	}
	return result, nil
}

// handleGetAmbientModules returns the symbols of the project's ambient module
// declarations, i.e. every global whose name is a quoted module specifier.
func (s *Session) handleGetAmbientModules(ctx context.Context, params *GetIntrinsicTypeParams) ([]*SymbolResponse, error) {
	setup, err := s.setupChecker(ctx, params.Snapshot, params.Project)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	modules := setup.checker.GetAmbientModules()
	if len(modules) == 0 {
		return nil, nil
	}
	slices.SortFunc(modules, setup.checker.CompareSymbols)

	results := make([]*SymbolResponse, len(modules))
	for i, module := range modules {
		results[i] = setup.newSymbolResponse(module)
	}
	return results, nil
}

// languageServiceSetup bundles the state a language service handler needs: the
// resolved snapshot/program/file plus the service itself.
type languageServiceSetup struct {
	sd          *snapshotData
	snapshot    *project.Snapshot
	program     *compiler.Program
	sourceFile  *ast.SourceFile
	langSvc     *ls.LanguageService
	documentURI lsproto.DocumentUri
	done        func()
}

// setupLanguageServiceForFile resolves a snapshot, project, and file, and builds
// a language service scoped to that file.
//
// Fixes that add imports need the snapshot's auto-import registry prepared for
// the file; autoImports requests that, at the cost of building the registry.
// Callers must call done() to release the prepared snapshot.
func (s *Session) setupLanguageServiceForFile(ctx context.Context, snapshot SnapshotID, project ProjectID, file DocumentIdentifier, autoImports bool) (*languageServiceSetup, error) {
	sd, err := s.getSnapshotData(snapshot)
	if err != nil {
		return nil, err
	}
	program, err := sd.getProgram(project)
	if err != nil {
		return nil, err
	}
	fileName := file.ToFileName()
	sourceFile := program.GetSourceFile(fileName)
	if sourceFile == nil {
		return nil, fmt.Errorf("%w: source file not found: %v", ErrClientError, file)
	}

	documentURI := file.ToURI(s.projectSession.GetCurrentDirectory())
	projectPath := parseProjectHandle(project)
	workingSnapshot := sd.snapshot
	done := func() {}

	if autoImports {
		if registry := workingSnapshot.AutoImportRegistry(); registry == nil ||
			!registry.IsPreparedForImportingFile(sourceFile.FileName(), projectPath, workingSnapshot.UserPreferences()) {
			prepared := s.projectSession.GetSnapshotWithAutoImports(ctx, workingSnapshot, documentURI)
			done = func() { prepared.Deref(s.projectSession) }
			workingSnapshot = prepared

			proj := workingSnapshot.ProjectCollection.GetProjectByPath(projectPath)
			if proj == nil {
				done()
				return nil, fmt.Errorf("%w: project %s not found", ErrClientError, projectPath)
			}
			program = proj.GetProgram()
			if program == nil {
				done()
				return nil, fmt.Errorf("%w: project has no program", ErrClientError)
			}
			sourceFile = program.GetSourceFile(fileName)
			if sourceFile == nil {
				done()
				return nil, fmt.Errorf("%w: source file not found: %v", ErrClientError, file)
			}
		}
	}

	proj := workingSnapshot.ProjectCollection.GetProjectByPath(projectPath)
	if proj == nil {
		done()
		return nil, fmt.Errorf("%w: project %s not found", ErrClientError, projectPath)
	}

	return &languageServiceSetup{
		sd:          sd,
		snapshot:    workingSnapshot,
		program:     program,
		sourceFile:  sourceFile,
		langSvc:     ls.NewLanguageService(proj.ID(), program, workingSnapshot, fileName),
		documentURI: documentURI,
		done:        done,
	}, nil
}

// toLSPPosition converts a character offset in the setup's file to an LSP position.
func (setup *languageServiceSetup) toLSPPosition(position int) lsproto.Position {
	return posToLSP(setup.snapshot.Converters(), 
		setup.sourceFile,
		core.TextPos(setup.sourceFile.GetPositionMap().UTF16ToUTF8(position)),
	)
}

// toAPIFileSpans flattens an LSP location response into offset-based spans.
// Definition-style responses are a union of one location, many locations, or
// links, so all three shapes are normalized here.
func (setup *languageServiceSetup) toAPIFileSpans(response lsproto.LocationOrLocationsOrDefinitionLinksOrNull) []*FileSpan {
	var locations []lsproto.Location
	switch {
	case response.Location != nil:
		locations = []lsproto.Location{*response.Location}
	case response.Locations != nil:
		locations = *response.Locations
	case response.DefinitionLinks != nil:
		for _, link := range *response.DefinitionLinks {
			if link == nil {
				continue
			}
			locations = append(locations, lsproto.Location{Uri: link.TargetUri, Range: link.TargetSelectionRange})
		}
	}

	converters := setup.snapshot.Converters()
	result := make([]*FileSpan, 0, len(locations))
	for _, location := range locations {
		fileName := location.Uri.FileName()
		sourceFile := setup.program.GetSourceFile(fileName)
		if sourceFile == nil {
			continue
		}
		positionMap := sourceFile.GetPositionMap()
		result = append(result, &FileSpan{
			FileName: fileName,
			Pos:      positionMap.UTF8ToUTF16(int(posFromLSP(converters, sourceFile, location.Range.Start))),
			End:      positionMap.UTF8ToUTF16(int(posFromLSP(converters, sourceFile, location.Range.End))),
		})
	}
	return result
}

// applyQuotePreference overrides the language service's quote preference for
// this request. An empty value leaves the snapshot's preference in place.
func applyQuotePreference(langSvc *ls.LanguageService, value string) error {
	switch lsutil.QuotePreference(value) {
	case lsutil.QuotePreferenceUnknown:
		return nil
	case lsutil.QuotePreferenceAuto, lsutil.QuotePreferenceDouble, lsutil.QuotePreferenceSingle:
		langSvc.SetQuotePreference(lsutil.QuotePreference(value))
		return nil
	default:
		return fmt.Errorf("%w: unknown quote preference %q", ErrClientError, value)
	}
}

// toFormatCodeSettings resolves the API's formatting options against the server's
// configured defaults. It goes through the LSP shape for the three fields that one
// carries, then applies the indent size, indent style and newline character on top —
// those the formatter reads but LSP has nowhere to put. IndentSize is applied last
// because FromLSFormatOptions derives it from the tab size.
func toFormatCodeSettings(base lsutil.FormatCodeSettings, options *FormattingOptions) lsutil.FormatCodeSettings {
	settings := lsutil.FromLSFormatOptions(base, toLSPFormattingOptions(options))
	if options == nil {
		return settings
	}
	if options.IndentSize != nil {
		settings.IndentSize = *options.IndentSize
	}
	if options.IndentStyle != nil {
		settings.IndentStyle = lsutil.IndentStyle(*options.IndentStyle)
	}
	if options.NewLineCharacter != nil {
		settings.NewLineCharacter = *options.NewLineCharacter
	}
	return settings
}

// toLSPFormattingOptions converts the API's formatting options into the LSP
// shape. The result is never nil: the formatter dereferences it, and omitted
// fields fall back to the defaults below.
func toLSPFormattingOptions(options *FormattingOptions) *lsproto.FormattingOptions {
	result := &lsproto.FormattingOptions{
		TabSize:      4,
		InsertSpaces: true,
	}
	if options == nil {
		return result
	}
	if options.TabSize != nil {
		result.TabSize = uint32(*options.TabSize)
	}
	if options.InsertSpaces != nil {
		result.InsertSpaces = *options.InsertSpaces
	}
	if options.TrimTrailingWhitespace != nil {
		result.TrimTrailingWhitespace = options.TrimTrailingWhitespace
	}
	return result
}

// posFromLSP converts an LSP position to a UTF-8 offset for a file the api serves
// directly, where the content mapping is identity.
func posFromLSP(c *lsconv.Converters, file *ast.SourceFile, pos lsproto.Position) core.TextPos {
	mapped := lsconv.FromLSPPositionForSourceFile(c, file, pos, spanmap.FeatureNone)
	if len(mapped) == 0 {
		return 0
	}
	return mapped[0].Position
}

// rangeToLSP converts an offset range to an LSP range for a plain api file, dropping the
// mapping fidelity (always exact for a file the api serves directly).
func rangeToLSP(c *lsconv.Converters, file *ast.SourceFile, r core.TextRange) lsproto.Range {
	rng, _ := c.ToLSPRange(file, r)
	return rng
}

// posToLSP converts a UTF-8 offset to an LSP position for a plain api file.
func posToLSP(c *lsconv.Converters, file *ast.SourceFile, pos core.TextPos) lsproto.Position {
	p, _ := c.ToLSPPosition(file, pos)
	return p
}
