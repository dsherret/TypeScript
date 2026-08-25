package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
	"github.com/microsoft/typescript-go/internal/tspath"
	"gotest.tools/v3/assert"
)

// TestSnapshotChangesMatchTheFileMapDiff checks what handleUpdateSnapshot reports as
// changed against comparing the two programs' whole file maps, which is what it used
// to do. The report is what the client invalidates, so a set that is not the diff
// costs a stale answer rather than a slow one, and every shape below is checked both
// ways after every single update.
func TestSnapshotChangesMatchTheFileMapDiff(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name  string
		files map[string]string
		open  []string
		steps []snapshotChangeStep
	}{
		{
			name:  "one file changed",
			files: map[string]string{"/p/a.ts": "export const a = 1;", "/p/b.ts": "export const b = 2;"},
			steps: []snapshotChangeStep{
				{write: map[string]string{"/p/a.ts": "export const a = 11;"}, changed: []string{"/p/a.ts"}, derived: true},
				{write: map[string]string{"/p/b.ts": "export const b = 22;"}, changed: []string{"/p/b.ts"}, derived: true},
				// the same text again: the project is not dirtied, so it keeps the
				// program it had and nothing may be reported
				{write: map[string]string{"/p/b.ts": "export const b = 22;"}, changed: []string{"/p/b.ts"}},
			},
		},
		{
			name:  "several files changed at once",
			files: map[string]string{"/p/a.ts": "export const a = 1;", "/p/b.ts": "export const b = 2;", "/p/c.ts": "export const c = 3;"},
			steps: []snapshotChangeStep{
				{
					write:   map[string]string{"/p/a.ts": "export const a = 11;", "/p/c.ts": "export const c = 33;"},
					changed: []string{"/p/a.ts", "/p/c.ts"},
					// two dirty files and no new root: neither incremental path takes it
					derived: false,
				},
			},
		},
		{
			name:  "a file created",
			files: map[string]string{"/p/a.ts": "export const a = 1;"},
			steps: []snapshotChangeStep{
				{write: map[string]string{"/p/b.ts": "export const b = 2;"}, roots: []string{"/p/b.ts"}, created: []string{"/p/b.ts"}, derived: true},
				{write: map[string]string{"/p/c.ts": "export const c = 3;"}, roots: []string{"/p/c.ts"}, created: []string{"/p/c.ts"}, derived: true},
			},
		},
		{
			name:  "a file created and another changed in the same update",
			files: map[string]string{"/p/a.ts": "export const a = 1;"},
			steps: []snapshotChangeStep{
				{
					write:   map[string]string{"/p/b.ts": "export const b = 2;", "/p/a.ts": "export const a = 11;"},
					roots:   []string{"/p/b.ts"},
					created: []string{"/p/b.ts"},
					changed: []string{"/p/a.ts"},
					derived: true,
				},
				{
					write:   map[string]string{"/p/c.ts": "export const c = 3;", "/p/a.ts": "export const a = 111;", "/p/b.ts": "export const b = 22;"},
					roots:   []string{"/p/c.ts"},
					created: []string{"/p/c.ts"},
					changed: []string{"/p/a.ts", "/p/b.ts"},
					derived: true,
				},
			},
		},
		{
			name:  "a file created that an import was waiting for",
			files: map[string]string{"/p/a.ts": `import { b } from "./b"; export const a = b;`},
			steps: []snapshotChangeStep{
				// the import probed for the file, so the addition is refused and the program is built again
				{write: map[string]string{"/p/b.ts": "export const b = 2;"}, roots: []string{"/p/b.ts"}, created: []string{"/p/b.ts"}, derived: false},
			},
		},
		{
			name:  "a file deleted",
			files: map[string]string{"/p/a.ts": "export const a = 1;", "/p/b.ts": "export const b = 2;"},
			steps: []snapshotChangeStep{
				{remove: []string{"/p/b.ts"}, dropRoots: []string{"/p/b.ts"}, deleted: []string{"/p/b.ts"}, derived: true},
			},
		},
		{
			name: "a file deleted that another imports",
			files: map[string]string{
				"/p/a.ts":   `import { dep } from "./dep"; export const a = dep;`,
				"/p/dep.ts": "export const dep = 3;",
			},
			steps: []snapshotChangeStep{
				{remove: []string{"/p/dep.ts"}, dropRoots: []string{"/p/dep.ts"}, deleted: []string{"/p/dep.ts"}},
			},
		},
		{
			name:  "a deletion, a creation and a change at once",
			files: map[string]string{"/p/a.ts": "export const a = 1;", "/p/b.ts": "export const b = 2;", "/p/c.ts": "export const c = 3;"},
			steps: []snapshotChangeStep{
				{
					write:     map[string]string{"/p/d.ts": "export const d = 4;", "/p/a.ts": "export const a = 11;"},
					remove:    []string{"/p/c.ts"},
					roots:     []string{"/p/d.ts"},
					dropRoots: []string{"/p/c.ts"},
					created:   []string{"/p/d.ts"},
					changed:   []string{"/p/a.ts"},
					deleted:   []string{"/p/c.ts"},
					derived:   true,
				},
			},
		},
		{
			name:  "a compiler option changed",
			files: map[string]string{"/p/a.ts": "export const a = 1;"},
			steps: []snapshotChangeStep{
				{options: map[string]any{"strict": true}, derived: false},
				{write: map[string]string{"/p/a.ts": "export const a = 11;"}, changed: []string{"/p/a.ts"}, derived: true},
				{options: map[string]any{"target": "es5"}, derived: false},
			},
		},
		{
			name:  "a compiler option changed alongside an edit and a creation",
			files: map[string]string{"/p/a.ts": "export const a = 1;"},
			steps: []snapshotChangeStep{
				{
					write:   map[string]string{"/p/b.ts": "export const b = 2;", "/p/a.ts": "export const a = 11;"},
					roots:   []string{"/p/b.ts"},
					options: map[string]any{"strict": true},
					created: []string{"/p/b.ts"},
					changed: []string{"/p/a.ts"},
				},
			},
		},
		{
			name: "a second project opened and closed",
			files: map[string]string{
				"/p/a.ts":          "export const a = 1;",
				"/q/tsconfig.json": `{"compilerOptions":{"allowJs":true},"files":["/q/x.ts"]}`,
				"/q/x.ts":          "export const x = 1;",
			},
			steps: []snapshotChangeStep{
				{open: []string{"/q/tsconfig.json"}},
				{write: map[string]string{"/q/x.ts": "export const x = 11;"}, changed: []string{"/q/x.ts"}},
				{write: map[string]string{"/p/a.ts": "export const a = 11;"}, changed: []string{"/p/a.ts"}, derived: true},
				{closeProjects: []string{"/q/tsconfig.json"}},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			s := newSnapshotChangesSession(t, testCase.files)
			defer s.close()
			for i, step := range testCase.steps {
				s.run(t, i, step)
			}
		})
	}
}

// TestTemporarySnapshotChangesMatchTheFileMapDiff checks the same thing for the
// temporary snapshot path, which diffs against a base the caller names rather than
// against the snapshot before it.
func TestTemporarySnapshotChangesMatchTheFileMapDiff(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newSnapshotChangesSession(t, map[string]string{
		"/p/a.ts": "export const a = 1;",
		"/p/b.ts": "export const b = 2;",
	})
	defer s.close()

	base := s.latest()
	response, err := s.session.handleUpdateTemporarySnapshot(context.Background(), &UpdateTemporarySnapshotParams{
		Snapshot: snapshotHandle(base),
		File:     DocumentIdentifier{FileName: "/p/a.ts"},
		NewText:  "export const a = 111;",
	})
	assert.NilError(t, err)
	next := s.session.snapshots[response.Snapshot].snapshot
	assertSameChanges(t, "changed text", response.Changes, referenceSnapshotChanges(base, next))
	assert.Assert(t, len(response.Changes.ChangedProjects) == 1)
	assert.Assert(t, s.tookFastPath(base, next))

	// overriding a file with the text it already has changes nothing, so nothing may
	// be reported changed
	response, err = s.session.handleUpdateTemporarySnapshot(context.Background(), &UpdateTemporarySnapshotParams{
		Snapshot: snapshotHandle(base),
		File:     DocumentIdentifier{FileName: "/p/a.ts"},
		NewText:  "export const a = 1;",
	})
	assert.NilError(t, err)
	next = s.session.snapshots[response.Snapshot].snapshot
	assertSameChanges(t, "unchanged text", response.Changes, referenceSnapshotChanges(base, next))
	assert.Equal(t, len(response.Changes.ChangedProjects), 0)
}

type snapshotChangeStep struct {
	// write and remove change the file system before the update is asked for.
	write  map[string]string
	remove []string
	// roots and dropRoots change the first project's config file list.
	roots     []string
	dropRoots []string
	// options are merged into the first project's compiler options.
	options map[string]any
	// created, changed and deleted are what the update is told about.
	created []string
	changed []string
	deleted []string
	// open and closeProjects change which projects the session holds open.
	open          []string
	closeProjects []string
	// derived is whether the main project's new program is expected to be able to say
	// what it changed, rather than leaving the two file maps to be compared.
	derived bool
}

const snapshotChangesConfigFileName = "/p/tsconfig.json"

type snapshotChangesSession struct {
	session *Session
	project *project.Session
	utils   *projecttestutil.SessionUtils
	roots   []string
	options map[string]any
	opened  []string
}

func newSnapshotChangesSession(t *testing.T, files map[string]string) *snapshotChangesSession {
	t.Helper()
	s := &snapshotChangesSession{
		options: map[string]any{"allowJs": true},
		opened:  []string{snapshotChangesConfigFileName},
	}
	initial := map[string]any{}
	for name, text := range files {
		initial[name] = text
		if strings.HasPrefix(name, "/p/") && !strings.HasSuffix(name, "tsconfig.json") {
			s.roots = append(s.roots, name)
		}
	}
	slices.Sort(s.roots)
	initial[snapshotChangesConfigFileName] = s.configText()
	s.project, s.utils = projecttestutil.SetupWithOptions(initial, &project.SessionOptions{
		CurrentDirectory:   "/",
		DefaultLibraryPath: bundled.LibPath(),
		PositionEncoding:   lsproto.PositionEncodingKindUTF8,
	})
	s.session = NewSession(s.project, nil)
	_, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		OpenProjects: identifiers(s.opened...),
	})
	assert.NilError(t, err)
	return s
}

func (s *snapshotChangesSession) close() {
	s.session.Close()
	s.project.Close()
}

func (s *snapshotChangesSession) configText() string {
	text, _ := json.Marshal(map[string]any{"compilerOptions": s.options, "files": s.roots})
	return string(text)
}

func (s *snapshotChangesSession) latest() *project.Snapshot {
	return s.session.snapshots[s.session.latestSnapshot].snapshot
}

// run applies a step and checks the changes it was reported against the ones a full
// diff of the two snapshots produces.
func (s *snapshotChangesSession) run(t *testing.T, index int, step snapshotChangeStep) {
	t.Helper()
	for name, text := range step.write {
		assert.NilError(t, s.utils.FS().WriteFile(name, text))
	}
	for _, name := range step.remove {
		assert.NilError(t, s.utils.FS().Remove(name))
	}

	changed := slices.Clone(step.changed)
	if len(step.roots) > 0 || len(step.dropRoots) > 0 || len(step.options) > 0 {
		s.roots = append(s.roots, step.roots...)
		s.roots = slices.DeleteFunc(s.roots, func(name string) bool { return slices.Contains(step.dropRoots, name) })
		for key, value := range step.options {
			s.options[key] = value
		}
		assert.NilError(t, s.utils.FS().WriteFile(snapshotChangesConfigFileName, s.configText()))
		changed = append(changed, snapshotChangesConfigFileName)
	}
	for _, name := range step.open {
		if !slices.Contains(s.opened, name) {
			s.opened = append(s.opened, name)
		}
	}
	s.opened = slices.DeleteFunc(s.opened, func(name string) bool { return slices.Contains(step.closeProjects, name) })

	prev := s.latest()
	response, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		FileChanges: &APIFileChanges{
			Created: identifiers(step.created...),
			Changed: identifiers(changed...),
			Deleted: identifiers(step.deleted...),
		},
		OpenProjects: identifiers(s.opened...),
	})
	assert.NilError(t, err)
	next := s.session.snapshots[response.Snapshot].snapshot
	where := fmt.Sprintf("step %d", index)
	assertSameChanges(t, where, response.Changes, referenceSnapshotChanges(prev, next))
	// the check above passes whether or not the program answered, so say which of the
	// two ways the answer came from and have every case name the one it means to take
	assert.Equal(t, s.tookFastPath(prev, next), step.derived, where+" answered from the program")
}

// tookFastPath reports whether the main project's new program was able to say what it
// changed, which is what decides which of the two paths computeSnapshotChanges takes.
func (s *snapshotChangesSession) tookFastPath(prev *project.Snapshot, next *project.Snapshot) bool {
	oldProj := prev.ProjectCollection.ConfiguredProject(tspath.Path(snapshotChangesConfigFileName))
	newProj := next.ProjectCollection.ConfiguredProject(tspath.Path(snapshotChangesConfigFileName))
	if oldProj == nil || newProj == nil || oldProj.GetProgram() == newProj.GetProgram() {
		return false
	}
	_, _, ok := newProj.GetProgram().FilesChangedFrom(oldProj.GetProgram())
	return ok
}

// referenceSnapshotChanges is what computeSnapshotChanges did before a program could
// say what it changed: compare every project's whole file map, every time.
func referenceSnapshotChanges(prev *project.Snapshot, next *project.Snapshot) *SnapshotChanges {
	var changes SnapshotChanges
	collections.DiffOrderedMaps(
		prev.ProjectCollection.ProjectsByPath(),
		next.ProjectCollection.ProjectsByPath(),
		func(_ tspath.Path, _ *project.Project) {},
		func(_ tspath.Path, oldProj *project.Project) {
			changes.RemovedProjects = append(changes.RemovedProjects, ProjectHandle(oldProj))
		},
		func(_ tspath.Path, oldProj *project.Project, newProj *project.Project) {
			if oldProj.GetProgram() == newProj.GetProgram() {
				return
			}
			var oldFiles, newFiles map[tspath.Path]*ast.SourceFile
			if p := oldProj.GetProgram(); p != nil {
				oldFiles = p.FilesByPath()
			}
			if p := newProj.GetProgram(); p != nil {
				newFiles = p.FilesByPath()
			}
			var projectChanges ProjectFileChanges
			core.DiffMaps(
				oldFiles, newFiles,
				nil,
				func(path tspath.Path, _ *ast.SourceFile) {
					projectChanges.DeletedFiles = append(projectChanges.DeletedFiles, path)
				},
				func(path tspath.Path, _ *ast.SourceFile, _ *ast.SourceFile) {
					projectChanges.ChangedFiles = append(projectChanges.ChangedFiles, path)
				},
			)
			if len(projectChanges.ChangedFiles) > 0 || len(projectChanges.DeletedFiles) > 0 {
				if changes.ChangedProjects == nil {
					changes.ChangedProjects = make(map[ProjectID]*ProjectFileChanges)
				}
				changes.ChangedProjects[ProjectHandle(newProj)] = &projectChanges
			}
		},
	)
	return &changes
}

func assertSameChanges(t *testing.T, what string, actual *SnapshotChanges, expected *SnapshotChanges) {
	t.Helper()
	assert.Equal(t, formatSnapshotChanges(actual), formatSnapshotChanges(expected), what)
}

func formatSnapshotChanges(changes *SnapshotChanges) string {
	if changes == nil {
		return "<nil>"
	}
	var b strings.Builder
	removed := make([]string, 0, len(changes.RemovedProjects))
	for _, id := range changes.RemovedProjects {
		removed = append(removed, string(id))
	}
	slices.Sort(removed)
	fmt.Fprintf(&b, "removed projects: %s\n", strings.Join(removed, ", "))
	ids := make([]string, 0, len(changes.ChangedProjects))
	for id := range changes.ChangedProjects {
		ids = append(ids, string(id))
	}
	slices.Sort(ids)
	for _, id := range ids {
		projectChanges := changes.ChangedProjects[ProjectID(id)]
		fmt.Fprintf(&b, "%s\n  changed: %s\n  deleted: %s\n", id, joinPaths(projectChanges.ChangedFiles), joinPaths(projectChanges.DeletedFiles))
	}
	return b.String()
}

func joinPaths(paths []tspath.Path) string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, string(path))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
