package api

import (
	"context"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

// TestCallbackFSAnswersBundledPathsItself pins that the lib files embedded in this
// executable are read from the embedded file system rather than asked of the client.
//
// A client that answers "don't know" would leave the reads falling through to the base
// file system anyway, so the case that matters is a client that answers definitively:
// it has no bundled:///libs/lib.es5.d.ts, and saying so would leave the program with no
// default library at all. Every read below is one the compiler makes by default.
func TestCallbackFSAnswersBundledPathsItself(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	fs := newCallbackFS(bundled.WrapFS(vfstest.FromMap(map[string]any{
		"/p/a.ts": "export const a = 1;",
	}, true)), []string{
		callbackReadFile,
		callbackFileExists,
		callbackDirectoryExists,
		callbackGetAccessibleEntries,
		callbackRealpath,
	})
	client := &notFoundConn{}
	fs.SetConnection(context.Background(), client)

	const libFile = "bundled:///libs/lib.es5.d.ts"

	contents, ok := fs.ReadFile(libFile)
	assert.Assert(t, ok, "the embedded lib file should be readable")
	assert.Assert(t, len(contents) > 0)
	assert.Assert(t, fs.FileExists(libFile))
	assert.Assert(t, fs.DirectoryExists(bundled.LibPath()))
	assert.Equal(t, fs.Realpath(libFile), libFile)
	assert.Assert(t, len(fs.GetAccessibleEntries(bundled.LibPath()).Files) > 0)
	assert.Equal(t, client.calls, 0, "the client should not have been asked about a bundled path")

	// every other path is still the client's to answer, which is what lets a
	// caller-supplied library folder be read through the callbacks
	_, ok = fs.ReadFile("/p/a.ts")
	assert.Assert(t, !ok, "the client's answer should be taken for a path it owns")
	assert.Assert(t, client.calls > 0)
}

// notFoundConn is a client that definitively reports every path as missing.
type notFoundConn struct {
	calls int
}

var _ ipc.Conn = (*notFoundConn)(nil)

func (c *notFoundConn) Call(ctx context.Context, method string, params any) (json.Value, error) {
	c.calls++
	switch method {
	case callbackReadFile:
		return json.Value(`{"content":null}`), nil
	case callbackRealpath:
		return json.Value(`""`), nil
	case callbackGetAccessibleEntries:
		return json.Value(`{"files":[],"directories":[]}`), nil
	default:
		return json.Value(`false`), nil
	}
}

func (c *notFoundConn) Run(ctx context.Context) error { return nil }

func (c *notFoundConn) Notify(ctx context.Context, method string, params any) error { return nil }
