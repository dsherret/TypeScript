package api

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/microsoft/typescript-go/internal/json"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/vfs"
)

// InProcessServer runs an API session in-process, without any transport.
//
// It is the transport-less analogue of StdioServer: instead of reading framed
// messages off a pipe, callers dispatch one request at a time synchronously via
// HandleRequest. Filesystem operations are delegated to the supplied Conn (for
// example, a WebAssembly host bridge that answers reads from an in-memory file
// system), matching the callback protocol used by the STDIO server.
type InProcessServer struct {
	session *Session
}

// InProcessServerOptions configures an InProcessServer.
type InProcessServerOptions struct {
	// FS is the base filesystem. Bundled library paths should already be
	// redirected to the embedded FS (e.g. via bundled.WrapFS).
	FS vfs.FS
	// Cwd is the current working directory. Required.
	Cwd string
	// DefaultLibraryPath is the directory under which the bundled lib.*.d.ts
	// files live.
	DefaultLibraryPath string
	// Callbacks names the filesystem operations delegated to the client via
	// Conn.Call. Empty means the base FS is used directly with no delegation.
	Callbacks []string
	// Conn receives filesystem callbacks. Required when Callbacks is non-empty.
	Conn Conn
}

// NewInProcessServer creates a new in-process API server. The provided context
// is used as the session's background context and for filesystem callbacks.
func NewInProcessServer(ctx context.Context, options *InProcessServerOptions) *InProcessServer {
	if options.Cwd == "" {
		panic("InProcessServerOptions.Cwd is required")
	}

	ctx = lsproto.WithClientCapabilities(ctx, apiClientCapabilities())

	fs := options.FS

	// resolveModuleName is a callback but not a filesystem one, so it is taken out
	// before the rest are handed to callbackFS.
	fsCallbacks := make([]string, 0, len(options.Callbacks))
	resolvesModuleNames := false
	for _, callback := range options.Callbacks {
		if callback == callbackResolveModuleName {
			resolvesModuleNames = true
			continue
		}
		fsCallbacks = append(fsCallbacks, callback)
	}

	// Wrap the base FS with callbackFS if callbacks are requested.
	var callbackFS *callbackFS
	if len(fsCallbacks) > 0 {
		callbackFS = newCallbackFS(fs, fsCallbacks)
		fs = callbackFS
	}

	projectSession := project.NewSession(&project.SessionInit{
		BackgroundCtx: ctx,
		FS:            fs,
		Options: &project.SessionOptions{
			CurrentDirectory:   options.Cwd,
			DefaultLibraryPath: options.DefaultLibraryPath,
			PositionEncoding:   lsproto.PositionEncodingKindUTF8,
			LoggingEnabled:     false,
			// An API client owns the project's file list the way tsserver owns an
			// inferred project's, so a file it names is in the project whatever its
			// extension — the same allowance NewInferredProject makes.
			AllowNonTsExtensions: true,
			ResolveModuleName:    newCallbackModuleResolver(ctx, options.Conn, resolvesModuleNames),
		},
	})

	session := NewSession(projectSession, &SessionOptions{
		UseBinaryResponses: true,
	})

	if callbackFS != nil {
		callbackFS.SetConnection(ctx, options.Conn)
	}

	return &InProcessServer{session: session}
}

// HandleRequest dispatches a single API request synchronously and returns its
// encoded response. When isBinary is true the response is raw binary (e.g.
// encoded node data); otherwise it is JSON. Filesystem callbacks made while
// handling the request are routed through the configured Conn.
func (s *InProcessServer) HandleRequest(ctx context.Context, method string, params json.Value) (response []byte, isBinary bool, err error) {
	// Recover panics and report them as errors. Without this a panic in a
	// handler aborts the whole runtime, which for the WebAssembly build means
	// the module is permanently unusable rather than just failing one request.
	defer func() {
		if r := recover(); r != nil {
			response = nil
			isBinary = false
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()

	result, err := s.session.HandleRequest(ctx, method, params)
	if err != nil {
		return nil, false, err
	}
	if raw, ok := result.(RawBinary); ok {
		return []byte(raw), true, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, false, err
	}
	return encoded, false, nil
}

// Close releases the underlying session's resources.
func (s *InProcessServer) Close() {
	s.session.Close()
}

// apiClientCapabilities describes what an API client supports, so that
// capability-gated language service behaviour is a deliberate choice rather than
// whatever the zero value happens to mean.
//
// Everything is off. The API returns plain per-file edit lists, so it cannot
// carry a WorkspaceEdit's DocumentChanges — neither the versioned TextDocumentEdits
// nor the create/rename/delete-file resource operations that come with them.
// Turning DocumentChanges on without also extending FileTextEdits would make
// rename and code fixes return document changes the handlers do not read; they
// reject that explicitly instead of silently truncating.
func apiClientCapabilities() *lsproto.ResolvedClientCapabilities {
	return &lsproto.ResolvedClientCapabilities{}
}
