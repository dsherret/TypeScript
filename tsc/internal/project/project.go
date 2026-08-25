package project

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ls"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/project/ata"
	"github.com/microsoft/TypeScript/tsc/internal/project/logging"
	"github.com/microsoft/TypeScript/tsc/internal/tsoptions"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

const (
	inferredProjectName = "/dev/null/inferred" // lowercase so toPath is a no-op regardless of settings
	hr                  = "-----------------------------------------------"
)

//go:generate go tool golang.org/x/tools/cmd/stringer -type=Kind -trimprefix=Kind -output=project_stringer_generated.go
//go:generate npx dprint fmt project_stringer_generated.go

type Kind int

const (
	KindInferred Kind = iota
	KindConfigured
)

type ProgramUpdateKind int

const (
	ProgramUpdateKindNone ProgramUpdateKind = iota
	ProgramUpdateKindCloned
	ProgramUpdateKindSameFileNames
	ProgramUpdateKindNewFiles
)

type PendingReload int

const (
	PendingReloadNone PendingReload = iota
	PendingReloadFileNames
	PendingReloadFull
)

// Project represents a TypeScript project.
// If changing struct fields, also update the Clone method.
type Project struct {
	Kind             Kind
	currentDirectory string
	configFileName   string
	configFilePath   tspath.Path

	dirty bool
	// dirtyFiles are the files whose text changed since Program was built and
	// deletedFiles the ones that went away, and they only describe the project's
	// dirtiness while dirtyFilesKnown. That goes false as soon as a change arrives
	// that no list of files accounts for — a package.json, a file the program looked
	// for and did not find — because each of those changes what the files around it
	// mean, and the program has to be built again rather than have those files
	// swapped in it.
	//
	// A deletion is listed apart from the rest because it is only ever answerable
	// together with the root file list: a file the program holds may leave it only by
	// being dropped as a root in the same update, which is what says nothing else was
	// relying on it. See Project.updateRootFilesInProgram.
	dirtyFiles      []tspath.Path
	deletedFiles    []tspath.Path
	dirtyFilesKnown bool

	host                            *compilerHost
	CommandLine                     *tsoptions.ParsedCommandLine
	commandLineWithTypingsFiles     *tsoptions.ParsedCommandLine
	commandLineWithTypingsFilesOnce sync.Once
	// apiRootFiles are root file names an API client named for this project directly
	// rather than through its config, in the order they were added.
	apiRootFiles                    []string
	commandLineWithAPIRootFiles     *tsoptions.ParsedCommandLine
	commandLineWithAPIRootFilesOnce sync.Once
	Program                         *compiler.Program
	// The kind of update that was performed on the program last time it was updated.
	ProgramUpdateKind ProgramUpdateKind
	// The ID of the snapshot that created the program stored in this project.
	ProgramLastUpdate uint64
	// Set of projects that this project could be referencing.
	// Only set before actually loading config file to get actual project references
	potentialProjectReferences *collections.Set[tspath.Path]

	programFilesWatch         *WatchedFiles[*collections.SyncSet[tspath.Path]]
	typingsWatch              *WatchedFiles[PatternsAndIgnored]
	contentMapperWatch        *WatchedFiles[[]string]
	contentMapperWatchedFiles *collections.Set[tspath.Path]

	checkerPool *checkerPool

	// installedTypingsInfo is the value of `project.ComputeTypingsInfo()` that was
	// used during the most recently completed typings installation.
	installedTypingsInfo *ata.TypingsInfo
	// typingsFiles are the root files added by the typings installer.
	typingsFiles []string
}

var _ ls.Project = (*Project)(nil)

func NewConfiguredProject(
	configFileName string,
	configFilePath tspath.Path,
	builder *ProjectCollectionBuilder,
	logger *logging.LogTree,
) *Project {
	return NewProject(configFileName, KindConfigured, tspath.GetDirectoryPath(configFileName), builder, logger)
}

func NewInferredProject(
	currentDirectory string,
	compilerOptions *core.CompilerOptions,
	rootFileNames []string,
	contentMappers []*contentmapper.Mapper,
	builder *ProjectCollectionBuilder,
	logger *logging.LogTree,
) *Project {
	p := NewProject(inferredProjectName, KindInferred, currentDirectory, builder, logger)
	if compilerOptions == nil {
		compilerOptions = &core.CompilerOptions{
			AllowJs:                    core.TSTrue,
			Module:                     core.ModuleKindESNext,
			ModuleResolution:           core.ModuleResolutionKindBundler,
			Target:                     core.ScriptTargetLatestStandard,
			Jsx:                        core.JsxEmitReactJSX,
			AllowImportingTsExtensions: core.TSTrue,
			StrictNullChecks:           core.TSTrue,
			StrictFunctionTypes:        core.TSTrue,
			SourceMap:                  core.TSTrue,
			AllowNonTsExtensions:       core.TSTrue,
			ResolveJsonModule:          core.TSTrue,
		}
	}
	p.CommandLine = newInferredProjectCommandLine(
		compilerOptions,
		rootFileNames,
		contentMappers,
		tspath.ComparePathsOptions{
			UseCaseSensitiveFileNames: builder.fs.fs.UseCaseSensitiveFileNames(),
			CurrentDirectory:          currentDirectory,
		},
	)
	return p
}

func newInferredProjectCommandLine(
	compilerOptions *core.CompilerOptions,
	rootFileNames []string,
	contentMappers []*contentmapper.Mapper,
	comparePathsOptions tspath.ComparePathsOptions,
) *tsoptions.ParsedCommandLine {
	commandLine := tsoptions.NewParsedCommandLine(compilerOptions, rootFileNames, comparePathsOptions)
	commandLine.ParsedConfig.ContentMappers = contentMappers
	return commandLine
}

func NewProject(
	configFileName string,
	kind Kind,
	currentDirectory string,
	builder *ProjectCollectionBuilder,
	logger *logging.LogTree,
) *Project {
	if logger != nil {
		logger.Log(fmt.Sprintf("Creating %sProject: %s, currentDirectory: %s", kind.String(), configFileName, currentDirectory))
	}
	project := &Project{
		configFileName:   configFileName,
		Kind:             kind,
		currentDirectory: currentDirectory,
		dirty:            true,
	}

	project.configFilePath = tspath.ToPath(configFileName, currentDirectory, builder.fs.fs.UseCaseSensitiveFileNames())
	project.programFilesWatch = NewWatchedFiles(
		"program files for "+configFileName,
		lsproto.WatchKindCreate|lsproto.WatchKindChange|lsproto.WatchKindDelete,
		lsproto.GetClientCapabilities(builder.ctx).Workspace.DidChangeWatchedFiles.RelativePatternSupport,
		createResolutionLookupGlobMapper(builder.sessionOptions.CurrentDirectory, builder.sessionOptions.DefaultLibraryPath, project.currentDirectory, builder.fs.fs.UseCaseSensitiveFileNames()),
	)
	if builder.sessionOptions.TypingsLocation != "" {
		project.typingsWatch = NewWatchedFiles(
			"typings installer files",
			lsproto.WatchKindCreate|lsproto.WatchKindChange|lsproto.WatchKindDelete,
			lsproto.GetClientCapabilities(builder.ctx).Workspace.DidChangeWatchedFiles.RelativePatternSupport,
			core.Identity,
		)
	}
	project.contentMapperWatch = NewWatchedFilesForPaths(
		"content mapper configuration files for "+configFileName,
		lsproto.WatchKindCreate|lsproto.WatchKindChange|lsproto.WatchKindDelete,
		lsproto.GetClientCapabilities(builder.ctx).Workspace.DidChangeWatchedFiles.RelativePatternSupport,
		builder.sessionOptions.CurrentDirectory,
		builder.sessionOptions.CurrentDirectory,
		builder.fs.fs.UseCaseSensitiveFileNames(),
	)
	return project
}

func (p *Project) Name() string {
	return p.configFileName
}

func (p *Project) CurrentDirectory() string {
	return p.currentDirectory
}

// DisplayName returns a short, human-readable name for the project,
// relative to the given workspace root directory.
// For configured projects, this is the config file path made relative.
// For inferred projects, this is the last component of the current directory.
func (p *Project) DisplayName(cwd string) string {
	if p.Kind == KindInferred {
		return tspath.GetBaseFileName(p.currentDirectory)
	}
	return tspath.ConvertToRelativePath(p.configFileName, tspath.ComparePathsOptions{
		CurrentDirectory: cwd,
	})
}

func (p *Project) ID() tspath.Path {
	return p.configFilePath
}

// ConfigFileName panics if Kind() is not KindConfigured.
func (p *Project) ConfigFileName() string {
	if p.Kind != KindConfigured {
		panic("ConfigFileName called on non-configured project")
	}
	return p.configFileName
}

// ConfigFilePath panics if Kind() is not KindConfigured.
func (p *Project) ConfigFilePath() tspath.Path {
	if p.Kind != KindConfigured {
		panic("ConfigFilePath called on non-configured project")
	}
	return p.configFilePath
}

func (p *Project) Id() tspath.Path {
	return p.configFilePath
}

func (p *Project) GetProgram() *compiler.Program {
	return p.Program
}

// GetProjectDiagnostics returns program diagnostics combined with any global
// diagnostics discovered during checking. These are the diagnostics reported on
// the tsconfig.json file.
func (p *Project) GetProjectDiagnostics(ctx context.Context) []*ast.Diagnostic {
	var globalDiags []*ast.Diagnostic
	if p.checkerPool != nil {
		globalDiags = p.checkerPool.GetGlobalDiagnostics()
	}
	return compiler.SortAndDeduplicateDiagnostics(slices.Concat(
		p.Program.GetConfigFileParsingDiagnostics(),
		p.Program.GetProgramDiagnostics(),
		globalDiags,
	))
}

func (p *Project) HasFile(fileName string) bool {
	return p.containsFile(p.toPath(fileName))
}

func (p *Project) containsFile(path tspath.Path) bool {
	return p.Program != nil && p.Program.GetSourceFileByPath(path) != nil
}

func (p *Project) IsSourceFromProjectReference(path tspath.Path) bool {
	return p.Program != nil && p.Program.IsSourceFromProjectReference(path)
}

func (p *Project) Clone() *Project {
	return &Project{
		Kind:             p.Kind,
		currentDirectory: p.currentDirectory,
		configFileName:   p.configFileName,
		configFilePath:   p.configFilePath,

		dirty:           p.dirty,
		dirtyFiles:      p.dirtyFiles,
		deletedFiles:    p.deletedFiles,
		dirtyFilesKnown: p.dirtyFilesKnown,

		host:                        p.host,
		CommandLine:                 p.CommandLine,
		commandLineWithTypingsFiles: p.commandLineWithTypingsFiles,
		apiRootFiles:                p.apiRootFiles,
		commandLineWithAPIRootFiles: p.commandLineWithAPIRootFiles,
		Program:                     p.Program,
		ProgramUpdateKind:           ProgramUpdateKindNone,
		ProgramLastUpdate:           p.ProgramLastUpdate,
		potentialProjectReferences:  p.potentialProjectReferences,

		programFilesWatch:         p.programFilesWatch,
		typingsWatch:              p.typingsWatch,
		contentMapperWatch:        p.contentMapperWatch,
		contentMapperWatchedFiles: p.contentMapperWatchedFiles,

		checkerPool: p.checkerPool,

		installedTypingsInfo: p.installedTypingsInfo,
		typingsFiles:         p.typingsFiles,
	}
}

// SetCommandLine reassigns the project's command line and resets all state derived
// from it. The project is marked dirty, since the program was built from the command
// line it replaces. It also resets:
//   - the memoized command lines augmented with typings files and with API root files
//     (and their sync.Onces, so both are rebuilt from the new command line on next
//     access);
//   - potentialProjectReferences, the pre-load placeholder derived from the old
//     command line (always nil for inferred projects, which have no project references).
//
// The API root files themselves are deliberately kept: they belong to the project
// rather than to its config, and rewriting the config is exactly what a client changing
// a compiler option does.
//
// What changed about the files is left alone: the two are independent, and comparing
// the command lines is how CreateProgram tells a root file added from anything else.
func (p *Project) SetCommandLine(commandLine *tsoptions.ParsedCommandLine) {
	p.CommandLine = commandLine
	p.commandLineWithTypingsFiles = nil
	p.commandLineWithTypingsFilesOnce = sync.Once{}
	p.commandLineWithAPIRootFiles = nil
	p.commandLineWithAPIRootFilesOnce = sync.Once{}
	p.potentialProjectReferences = nil
	p.dirty = true
}

// RootFileNames are the root files the project was asked to hold: the ones its config
// named, followed by the ones an API client named for it directly. Files the typings
// installer added are left out, since those are the program's rather than the project's.
func (p *Project) RootFileNames() []string {
	if len(p.apiRootFiles) == 0 {
		return p.CommandLine.FileNames()
	}
	return slices.Concat(p.CommandLine.FileNames(), p.apiRootFiles)
}

// setAPIRootFiles replaces the root files an API client named for this project. The
// slice is taken rather than copied and must never be appended to in place: a snapshot
// clones the map it came from, so the one behind this project is still holding it.
func (p *Project) setAPIRootFiles(fileNames []string) {
	p.apiRootFiles = fileNames
	p.commandLineWithAPIRootFiles = nil
	p.commandLineWithAPIRootFilesOnce = sync.Once{}
	p.dirty = true
}

// effectiveCommandLine is the project's config command line with the root files nothing
// in the config named appended: the typings installer's, then the ones an API client
// named. The API roots go last because appending to the end of the list is the only
// shape Program.AddRootFiles can extend, so a client adding a root is adding to the end.
// (A typings file arriving after an API root therefore inserts in the middle and costs
// one rebuild — correct, and rare enough to be worth the order.)
func (p *Project) effectiveCommandLine() *tsoptions.ParsedCommandLine {
	commandLine := p.getCommandLineWithTypingsFiles()
	if len(p.apiRootFiles) == 0 {
		return commandLine
	}
	p.commandLineWithAPIRootFilesOnce.Do(func() {
		if p.commandLineWithAPIRootFiles == nil {
			p.commandLineWithAPIRootFiles = commandLine.WithAdditionalRootFiles(p.apiRootFiles)
		}
	})
	return p.commandLineWithAPIRootFiles
}

// getCommandLineWithTypingsFiles returns the command line augmented with typing files if ATA is enabled.
func (p *Project) getCommandLineWithTypingsFiles() *tsoptions.ParsedCommandLine {
	if len(p.typingsFiles) == 0 {
		return p.CommandLine
	}

	// Check if ATA is enabled for this project
	typeAcquisition := p.GetTypeAcquisition()
	if typeAcquisition == nil || !typeAcquisition.Enable.IsTrue() {
		return p.CommandLine
	}

	p.commandLineWithTypingsFilesOnce.Do(func() {
		if p.commandLineWithTypingsFiles == nil {
			// Create an augmented command line that includes typing files
			originalRootNames := p.CommandLine.FileNames()
			newRootNames := make([]string, 0, len(originalRootNames)+len(p.typingsFiles))
			newRootNames = append(newRootNames, originalRootNames...)
			newRootNames = append(newRootNames, p.typingsFiles...)

			p.commandLineWithTypingsFiles = p.CommandLine.WithFileNames(newRootNames)
		}
	})
	return p.commandLineWithTypingsFiles
}

func (p *Project) setPotentialProjectReference(configFilePath tspath.Path) {
	if p.potentialProjectReferences == nil {
		p.potentialProjectReferences = &collections.Set[tspath.Path]{}
	} else {
		p.potentialProjectReferences = p.potentialProjectReferences.Clone()
	}
	p.potentialProjectReferences.Add(configFilePath)
}

func (p *Project) hasPotentialProjectReference(projectTreeRequest *ProjectTreeRequest) bool {
	if p.CommandLine != nil {
		for _, path := range p.CommandLine.ResolvedProjectReferencePaths() {
			if projectTreeRequest.IsProjectReferenced(p.toPath(path)) {
				return true
			}
		}
	} else if p.potentialProjectReferences != nil {
		for path := range p.potentialProjectReferences.Keys() {
			if projectTreeRequest.IsProjectReferenced(path) {
				return true
			}
		}
	}
	return false
}

type CreateProgramResult struct {
	Program    *compiler.Program
	UpdateKind ProgramUpdateKind
}

func (p *Project) CreateProgram() CreateProgramResult {
	updateKind := ProgramUpdateKindNewFiles
	var programCloned bool
	var fileNamesKnownToDiffer bool
	var newProgram *compiler.Program

	// Define a fresh CreateCheckerPool closure for this call. Each invocation of
	// CreateProgram must use its own closure so that concurrent goroutines cloning
	// the same project never share a captured variable through a stale closure
	// stored in the old program's options.
	createCheckerPool := func(program *compiler.Program) compiler.CheckerPool {
		return newCheckerPool(p.host.sessionOptions.CheckerPoolOptions, program, p.log)
	}

	// Create the command line, potentially augmented with typing files
	commandLine := p.getCommandLineWithTypingsFiles()
	if p.dirtyFilePath != "" && p.Program != nil && p.Program.CommandLine() == commandLine {
		var dirtyFile *ast.SourceFile
		newProgram, dirtyFile, programCloned = p.Program.UpdateProgram(p.dirtyFilePath, p.host, createCheckerPool)
		if programCloned {
			updateKind = ProgramUpdateKindCloned
			for _, file := range newProgram.SourceFiles() {
				// Use pointer identity: dirtyFile is the exact instance UpdateProgram acquired,
				// and it is the only file whose refcount is already accounted for.
				if file != dirtyFile && !file.IsContentMapperFailureStub() && !file.IsContentMapperSupplemental() {
					// UpdateProgram acquired the changed file only, so we need to ref everything else
					if file.ContentMapper() != "" {
						p.host.builder.contentMappedParseCache.Ref(contentMappedParseCacheKeyForFile(file))
					} else {
						p.host.builder.parseCache.Ref(parseCacheKeyForFile(file))
					}
				}
			}
			for _, file := range newProgram.DuplicateSourceFiles() {
				if !file.IsContentMapperFailureStub {
					if file.ContentMapper != "" {
						p.host.builder.contentMappedParseCache.Ref(contentMappedParseCacheKeyForDuplicate(file))
					} else {
						p.host.builder.parseCache.Ref(parseCacheKeyForDuplicate(file))
					}
				}
			}
		} else if dirtyFile != nil {
			// UpdateProgram always acquires the dirty file before deciding whether it can
			// reuse the old program. If it falls back to a full rebuild, release that
			// speculative acquire so the rebuilt program is the only remaining owner.
			if dirtyFile.ContentMapper() != "" {
				p.host.builder.contentMappedParseCache.Deref(contentMappedParseCacheKeyForFile(dirtyFile))
			} else {
				p.host.builder.parseCache.Deref(parseCacheKeyForFile(dirtyFile))
			}
		}
	} else {
		var typingsLocation string
		if p.GetTypeAcquisition().Enable.IsTrue() {
			typingsLocation = p.host.sessionOptions.TypingsLocation
		}
		newProgram = compiler.NewProgram(
			compiler.ProgramOptions{
				Host:                        p.host,
				Config:                      commandLine,
				UseSourceOfProjectReference: true,
				TypingsLocation:             typingsLocation,
				CreateCheckerPool:           createCheckerPool,
			},
		)
	}

	if !programCloned && !fileNamesKnownToDiffer && p.Program != nil && p.Program.HasSameFileNames(newProgram) {
		updateKind = ProgramUpdateKindSameFileNames
	}

	newProgram.BindSourceFiles()

	return CreateProgramResult{
		Program:    newProgram,
		UpdateKind: updateKind,
	}
}

// updateRootFilesInProgram builds the next program by changing the root files of the
// current one — adding the ones the new command line names beyond it and dropping the
// ones it no longer names — rather than building it again. It returns nil when that
// cannot be done, which leaves the caller to build one, and is what happens whenever the
// command line changed for any other reason — the compiler options, a project reference,
// roots that moved rather than arrived or left — or whenever the change would not
// produce the same program a build from scratch would have.
//
// Two conditions the compiler cannot check are here. An added file must be one no file
// already in the program went looking for, because a build from scratch would resolve
// that lookup to it now and the file that made it would mean something different; that
// is asked of the previous program's host, which recorded every path it read or probed,
// and it is asked before the new host is given that record to add to. And a file the
// program holds may only go away by being dropped as a root in the same update: one that
// vanished from the file system while the project still names it is a program that has
// to be built again, since what its name means now is a question about the file system
// rather than about the program.
func (p *Project) updateRootFilesInProgram(
	commandLine *tsoptions.ParsedCommandLine,
	createCheckerPool func(*compiler.Program) compiler.CheckerPool,
) *compiler.Program {
	if p.Program == nil || !p.dirtyFilesKnown {
		return nil
	}
	addedRootFileNames, removedRootFileNames, ok := p.Program.RootFileChangesFrom(commandLine)
	if !ok {
		return nil
	}
	oldHost, ok := p.Program.Host().(*compilerHost)
	if !ok || oldHost.sourceFS.seenFiles == nil || p.host.sourceFS.seenFiles == nil {
		return nil
	}
	for _, fileName := range addedRootFileNames {
		if oldHost.sourceFS.SeenFileOrMissingParentDirectory(p.toPath(fileName)) {
			return nil
		}
	}
	if len(p.deletedFiles) > 0 {
		var removedRootPaths collections.Set[tspath.Path]
		for _, fileName := range removedRootFileNames {
			removedRootPaths.Add(p.toPath(fileName))
		}
		for _, path := range p.deletedFiles {
			if !removedRootPaths.Has(path) {
				return nil
			}
		}
	}

	newProgram, acquired, ok := p.Program.UpdateRootFiles(commandLine, p.dirtyFiles, p.host, createCheckerPool)
	if ok {
		for _, file := range acquired {
			// the files that replaced changed ones were already in the program, so
			// only the ones this walk brought in have to be new to it
			if !slices.Contains(p.dirtyFiles, file.Path()) && oldHost.sourceFS.SeenFileOrMissingParentDirectory(file.Path()) {
				ok = false
				break
			}
		}
	}
	if !ok {
		for _, file := range acquired {
			p.host.builder.parseCache.Deref(NewParseCacheKey(file.ParseOptions(), file.Hash, file.ScriptKind))
		}
		return nil
	}

	// everything the new program did not acquire itself came from the old one, which
	// keeps its own reference until it is disposed
	acquiredFiles := make(map[*ast.SourceFile]struct{}, len(acquired))
	for _, file := range acquired {
		acquiredFiles[file] = struct{}{}
	}
	for _, file := range newProgram.SourceFiles() {
		if _, isNew := acquiredFiles[file]; !isNew {
			p.host.builder.parseCache.RefValue(file)
		}
	}
	for _, file := range newProgram.DuplicateSourceFiles() {
		p.host.builder.parseCache.Ref(NewParseCacheKey(file.ParseOptions, file.Hash, file.ScriptKind))
	}

	// the new host has only read what the change needed, and the program depends on
	// everything the one before it read, so the two records become one
	p.host.sourceFS.seenFiles.Range(func(path tspath.Path) bool {
		oldHost.sourceFS.seenFiles.Add(path)
		return true
	})
	if p.host.sourceFS.missingDirectories != nil && oldHost.sourceFS.missingDirectories != nil {
		p.host.sourceFS.missingDirectories.Range(func(path tspath.Path) bool {
			oldHost.sourceFS.missingDirectories.Add(path)
			return true
		})
		p.host.sourceFS.missingDirectories = oldHost.sourceFS.missingDirectories
	}
	p.host.sourceFS.seenFiles = oldHost.sourceFS.seenFiles
	return newProgram
}

func (p *Project) CloneWatchers() *WatchedFiles[*collections.SyncSet[tspath.Path]] {
	return p.programFilesWatch.Clone(p.host.sourceFS.seenFiles)
}

func (p *Project) log(msg string) {
	// !!!
}

func (p *Project) toPath(fileName string) tspath.Path {
	return tspath.ToPath(fileName, p.currentDirectory, p.host.FS().UseCaseSensitiveFileNames())
}

func (p *Project) print(writeFileNames bool, writeFileExplanation bool, builder *strings.Builder) string {
	builder.WriteString(fmt.Sprintf("\nProject '%s'\n", p.Name()))
	if p.Program == nil {
		builder.WriteString("\tFiles (0) NoProgram\n")
	} else {
		sourceFiles := p.Program.GetSourceFiles()
		builder.WriteString(fmt.Sprintf("\tFiles (%d)\n", len(sourceFiles)))
		if writeFileNames {
			for _, sourceFile := range sourceFiles {
				builder.WriteString("\t\t")
				builder.WriteString(sourceFile.FileName())
				builder.WriteString("\n")
			}
			// !!!
			// if writeFileExplanation {}
		}
	}
	builder.WriteString(hr)
	return builder.String()
}

// GetTypeAcquisition returns the type acquisition settings for this project.
func (p *Project) GetTypeAcquisition() *core.TypeAcquisition {
	if p.Kind == KindInferred {
		// For inferred projects, use default settings
		return &core.TypeAcquisition{
			Enable:                              core.TSTrue,
			Include:                             nil,
			Exclude:                             nil,
			DisableFilenameBasedTypeAcquisition: core.TSFalse,
		}
	}

	if p.CommandLine != nil {
		return p.CommandLine.TypeAcquisition()
	}

	return nil
}

// GetUnresolvedImports extracts unresolved imports from this project's program.
func (p *Project) GetUnresolvedImports() *collections.Set[string] {
	if p.Program == nil {
		return nil
	}

	return p.Program.GetUnresolvedImports()
}

// ShouldTriggerATA determines if ATA should be triggered for this project.
func (p *Project) ShouldTriggerATA(snapshotID uint64) bool {
	if p.Program == nil || p.CommandLine == nil {
		return false
	}

	typeAcquisition := p.GetTypeAcquisition()
	if typeAcquisition == nil || !typeAcquisition.Enable.IsTrue() {
		return false
	}

	if p.installedTypingsInfo == nil || p.ProgramLastUpdate == snapshotID && p.ProgramUpdateKind == ProgramUpdateKindNewFiles {
		return true
	}

	return !p.installedTypingsInfo.Equals(p.ComputeTypingsInfo())
}

func (p *Project) ComputeTypingsInfo() ata.TypingsInfo {
	return ata.TypingsInfo{
		CompilerOptions:   p.CommandLine.CompilerOptions(),
		TypeAcquisition:   p.GetTypeAcquisition(),
		UnresolvedImports: p.GetUnresolvedImports(),
	}
}
