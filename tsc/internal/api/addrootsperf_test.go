package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
)

// These are measurements rather than assertions, and only run under TSPERF=1:
//
//	TSPERF=1 TSPERF_N=1600 GOMAXPROCS=1 go test ./internal/api -run TestPerf -v
//
// They replay the request sequence ts-morph's document registry makes, so that the
// cost of adding root files to a project can be seen without a wasm build in the way.
// GOMAXPROCS=1 is what makes the profile a tree, and is also what the wasm build is.
const perfConfigFileName = "/perf/tsconfig.json"

func perfFileCount() int {
	n := 400
	if v := os.Getenv("TSPERF_N"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	return n
}

type perfHarness struct {
	session *Session
	utils   *projecttestutil.SessionUtils
	names   []string
	ctx     context.Context
}

func perfSetup(t *testing.T) (*perfHarness, func()) {
	if os.Getenv("TSPERF") == "" {
		t.Skip("set TSPERF=1")
	}
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}
	files := map[string]any{
		perfConfigFileName: `{"compilerOptions":{"allowJs":true,"singleThreaded":true},"files":[]}`,
	}
	// the same options internal/api/server.go opens a session with, which is what
	// ts-morph drives: no watching, no logging, no pushed diagnostics
	projectSession, utils := projecttestutil.SetupWithOptions(files, &project.SessionOptions{
		CurrentDirectory:   "/",
		DefaultLibraryPath: bundled.LibPath(),
		PositionEncoding:   lsproto.PositionEncodingKindUTF8,
	})
	session := NewSession(projectSession, nil)
	h := &perfHarness{session: session, utils: utils, ctx: context.Background()}
	_, err := session.handleUpdateSnapshot(h.ctx, &UpdateSnapshotParams{
		OpenProjects: []DocumentIdentifier{{FileName: perfConfigFileName}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, func() {
		session.Close()
		projectSession.Close()
	}
}

func (h *perfHarness) writeConfig() {
	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{"allowJs": true, "singleThreaded": true},
		"files":           h.names,
	})
	h.utils.FS().WriteFile(perfConfigFileName, string(text))
}

func (h *perfHarness) update(t *testing.T, changes *APIFileChanges) {
	_, err := h.session.handleUpdateSnapshot(h.ctx, &UpdateSnapshotParams{
		FileChanges:  changes,
		OpenProjects: []DocumentIdentifier{{FileName: perfConfigFileName}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPerfInterleavedCreateAndManipulate mirrors ts-morph's `createSourceFile` then `addClass` loop:
// each iteration creates a file, rewrites the config naming every file, and
// reports the previous iteration's manipulation as a change.
func TestPerfInterleavedCreateAndManipulate(t *testing.T) {
	h, done := perfSetup(t)
	defer done()
	n := perfFileCount()

	start := time.Now()
	for i := range n {
		name := fmt.Sprintf("/perf/b%d.ts", i)
		h.utils.FS().WriteFile(name, fmt.Sprintf("export const v%d = %d;", i, i))
		h.names = append(h.names, name)
		h.writeConfig()
		changes := &APIFileChanges{
			Created: []DocumentIdentifier{{FileName: name}},
			Changed: []DocumentIdentifier{{FileName: perfConfigFileName}},
		}
		if i > 0 {
			prev := fmt.Sprintf("/perf/b%d.ts", i-1)
			h.utils.FS().WriteFile(prev, fmt.Sprintf("export const v%d = %d;\n\nclass C%d {\n}\n", i-1, i-1, i-1))
			changes.Changed = append(changes.Changed, DocumentIdentifier{FileName: prev})
		}
		h.update(t, changes)
	}
	elapsed := time.Since(start)
	fmt.Printf("interleave n=%d total=%v per-file=%v\n", n, elapsed, elapsed/time.Duration(n))
}

// TestPerfCreateThenManipulate is the yardstick: create every file first (one config write),
// then manipulate each in turn.
func TestPerfCreateThenManipulate(t *testing.T) {
	h, done := perfSetup(t)
	defer done()
	n := perfFileCount()

	created := make([]DocumentIdentifier, 0, n)
	for i := range n {
		name := fmt.Sprintf("/perf/b%d.ts", i)
		h.utils.FS().WriteFile(name, fmt.Sprintf("export const v%d = %d;", i, i))
		h.names = append(h.names, name)
		created = append(created, DocumentIdentifier{FileName: name})
	}
	h.writeConfig()
	h.update(t, &APIFileChanges{
		Created: created,
		Changed: []DocumentIdentifier{{FileName: perfConfigFileName}},
	})

	start := time.Now()
	for i := range n {
		name := fmt.Sprintf("/perf/b%d.ts", i)
		h.utils.FS().WriteFile(name, fmt.Sprintf("export const v%d = %d;\n\nclass C%d {\n}\n", i, i, i))
		h.update(t, &APIFileChanges{Changed: []DocumentIdentifier{{FileName: name}}})
	}
	elapsed := time.Since(start)
	fmt.Printf("twoloop n=%d total=%v per-file=%v\n", n, elapsed, elapsed/time.Duration(n))
}
