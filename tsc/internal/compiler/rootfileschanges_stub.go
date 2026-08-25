package compiler

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/tsoptions"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

// RootFileChangesFrom and UpdateRootFiles are stubs: the incremental root
// add/remove-without-rebuild feature is deferred pending re-implementation against
// upstream's content-mapper file loader. Reporting no incremental update makes every
// caller fall back to a full program rebuild, which is correct, just not optimized.

func (p *Program) RootFileChangesFrom(newConfig *tsoptions.ParsedCommandLine) (added []string, removed []string, ok bool) {
	return nil, nil, false
}

func (p *Program) UpdateRootFiles(
	newConfig *tsoptions.ParsedCommandLine,
	changedFilePaths []tspath.Path,
	newHost CompilerHost,
	createCheckerPool func(*Program) CheckerPool,
) (*Program, []*ast.SourceFile, bool) {
	return nil, nil, false
}

// FilesChangedFrom is a stub (deferred incremental-roots): reporting no known change
// makes callers fall back to diffing the file maps themselves.
func (p *Program) FilesChangedFrom(other *Program) (changed []tspath.Path, removed []tspath.Path, ok bool) {
	return nil, nil, false
}
