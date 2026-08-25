//go:build wasip1

// Command tsgo-wasm builds the TypeScript API server as a WebAssembly reactor
// module (GOOS=wasip1, -buildmode=c-shared) that runs entirely in-process.
//
// Instead of talking to a spawned tsgo process over a pipe, a JS host drives the
// module synchronously:
//
//   - create_session(cwd) once, to build the API session. It returns 0 on
//     success and 1 on failure, with the reason in the response buffer.
//   - get_request_buffer(size) to obtain a shared buffer, write the method bytes
//     followed by the JSON payload into it, then call handle_request(methodLen,
//     payloadLen). The status is the return value (0 ok, 1 error); the response
//     bytes are read from response_ptr()/response_len(), and response_is_binary()
//     reports whether they are raw binary or JSON.
//   - close_session() to release the session's programs, snapshots and
//     background queue.
//
// Filesystem access is delegated back to the host via the ts_host.callback /
// ts_host.read_result imports, so the host provides a virtual file system (the
// same callback contract the STDIO server uses). Bundled lib.*.d.ts files are
// embedded in the module itself and never hit the host.
//
// The module is single-threaded: one request runs to completion per call, and
// re-entering an export while another is in flight is rejected rather than
// corrupting the shared buffers.
package main

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"unsafe"

	"github.com/microsoft/typescript-go/internal/api"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/json"
	"github.com/microsoft/typescript-go/internal/vfs/vfstest"
)

func main() {}

// Host imports. The host answers a filesystem callback by stashing the result
// and returning its length; the module then allocates a buffer and asks the
// host to copy the bytes in. Splitting it into two calls lets the result be any
// size while keeping ownership of the destination buffer on the Go side.
//
// A JS exception thrown out of an import would unwind these frames as a trap
// rather than a Go panic, skipping every deferred unlock and leaving the module
// permanently dead. The host therefore never throws: it reports failure by
// setting hostErrorFlag on the returned length, with the message as the result
// payload.
//
//go:wasmimport ts_host callback
func hostCallback(namePtr, nameLen, argPtr, argLen uint32) uint32

//go:wasmimport ts_host read_result
func hostReadResult(destPtr, destLen uint32)

// hostErrorFlag marks a callback return value as an error length rather than a
// result length. Lengths are bounded by the 32-bit address space, so the top bit
// is always free.
const hostErrorFlag uint32 = 1 << 31

// allFSCallbacks are the filesystem operations delegated to the host. Anything
// not delegated (and not an embedded lib) resolves against the empty base FS.
var allFSCallbacks = []string{"readFile", "fileExists", "directoryExists", "getAccessibleEntries", "realpath", "writeFile"}

var (
	ctx    = context.Background()
	server *api.InProcessServer

	// inCall guards the exported entry points. The shared request/response
	// buffers and the stashed host callback result are all single-slot, so
	// re-entering an export from inside a host callback would clobber the
	// in-flight request. Reject it instead.
	inCall bool

	// reqBuf holds the incoming method + payload for the current request.
	reqBuf []byte

	// respBuf holds the outgoing response until the next request overwrites it;
	// it is kept referenced so the pointer handed to the host stays valid.
	respBuf    []byte
	respPtr    uint32
	respLen    uint32
	respBinary uint32

	// callMu serializes host callbacks so two concurrently-scheduled goroutines
	// cannot interleave the callback/read_result pair and read each other's
	// stashed result. The module is single-threaded, so this only ever blocks a
	// goroutine until the in-flight callback completes.
	callMu sync.Mutex
)

// sessionOptions is the JSON payload create_session is given.
type sessionOptions struct {
	Cwd                string `json:"cwd"`
	DefaultLibraryPath string `json:"defaultLibraryPath"`
	// UseCaseSensitiveFileNames describes the host's file system. Nil keeps the
	// case-sensitive default; a host backed by a Windows or macOS disk says false
	// so the compiler resolves modules the way that disk does.
	UseCaseSensitiveFileNames *bool `json:"useCaseSensitiveFileNames"`
	// ResolveModuleName says the host answers module resolutions itself. Off by
	// default: with it on the compiler asks the host about every specifier, and a
	// host that did not mean to offer that would pay a callback per import.
	ResolveModuleName bool `json:"resolveModuleName"`
}

// create_session builds the API session from the JSON options in the request
// buffer. It must be called once before handle_request, and returns 0 on
// success or 1 on failure (with the reason in the response buffer).
//
//go:wasmexport create_session
func createSession(cwdPtr, cwdLen uint32) (status uint32) {
	if server != nil {
		setResponse([]byte("session already created"), false)
		return 1
	}
	// Construction can panic (an unknown callback name, an empty cwd), and an
	// unrecovered panic in an export kills the module for good.
	defer func() {
		if r := recover(); r != nil {
			server = nil
			setResponse(fmt.Appendf(nil, "panic creating session: %v\n%s", r, debug.Stack()), false)
			status = 1
		}
	}()

	var options sessionOptions
	if err := json.Unmarshal(readMem(cwdPtr, cwdLen), &options); err != nil {
		setResponse(fmt.Appendf(nil, "invalid session options: %v", err), false)
		return 1
	}
	if options.Cwd == "" {
		options.Cwd = "/"
	}
	// The libs live inside the module, so the host only names a directory of its
	// own when it wants its own copies read through the file system callbacks.
	defaultLibraryPath := options.DefaultLibraryPath
	if defaultLibraryPath == "" {
		defaultLibraryPath = bundled.LibPath()
	}
	useCaseSensitiveFileNames := options.UseCaseSensitiveFileNames == nil || *options.UseCaseSensitiveFileNames
	callbacks := allFSCallbacks
	if options.ResolveModuleName {
		callbacks = append(append([]string{}, allFSCallbacks...), "resolveModuleName")
	}
	base := bundled.WrapFS(vfstest.FromMap(map[string]string{}, useCaseSensitiveFileNames))
	server = api.NewInProcessServer(ctx, &api.InProcessServerOptions{
		FS:                 base,
		Cwd:                options.Cwd,
		DefaultLibraryPath: defaultLibraryPath,
		Callbacks:          callbacks,
		Conn:               wasmConn{},
	})
	setResponse(nil, false)
	return 0
}

// close_session releases the session's resources. Further requests fail until
// create_session is called again.
//
//go:wasmexport close_session
func closeSession() {
	if server == nil {
		return
	}
	s := server
	server = nil
	defer func() { _ = recover() }()
	s.Close()
}

// get_request_buffer ensures the shared request buffer holds at least size bytes
// and returns a pointer to its start for the host to write the request into.
//
//go:wasmexport get_request_buffer
func getRequestBuffer(size uint32) uint32 {
	// Grow on demand, but give a large buffer back once traffic drops to small
	// requests: linear memory never shrinks, so a single multi-megabyte payload
	// would otherwise raise the module's floor for its whole lifetime.
	if uint32(cap(reqBuf)) < size || (cap(reqBuf) > 1<<20 && uint32(cap(reqBuf)) > 4*size) {
		reqBuf = make([]byte, size)
	}
	reqBuf = reqBuf[:size]
	return bytesPtr(reqBuf)
}

// handle_request dispatches the request currently in the shared buffer: the
// first methodLen bytes are the method name, the next payloadLen bytes are the
// JSON payload. It returns 0 on success and 1 on error; the response is then
// available via response_ptr/response_len/response_is_binary.
//
//go:wasmexport handle_request
func handleRequest(methodLen, payloadLen uint32) uint32 {
	if inCall {
		setResponse([]byte("re-entrant handle_request: a request is already in flight"), false)
		return 1
	}
	if server == nil {
		setResponse([]byte("no session: create_session must be called first"), false)
		return 1
	}
	if methodLen+payloadLen > uint32(len(reqBuf)) || methodLen+payloadLen < methodLen {
		setResponse([]byte("invalid request lengths: method + payload exceeds the request buffer"), false)
		return 1
	}
	inCall = true
	defer func() { inCall = false }()

	// Release the previous response before building the next one so at most one
	// payload is pinned at a time.
	respBuf, respPtr, respLen, respBinary = nil, 0, 0, 0

	method := string(reqBuf[:methodLen])
	// The payload must be copied, not aliased: handlers may retain it (the echo
	// handler returns RawBinary(params) verbatim), and reqBuf is overwritten by
	// the next get_request_buffer call.
	payload := make([]byte, payloadLen)
	copy(payload, reqBuf[methodLen:methodLen+payloadLen])

	response, isBinary, err := server.HandleRequest(ctx, method, json.Value(payload))
	if err != nil {
		setResponse([]byte(err.Error()), false)
		return 1
	}
	setResponse(response, isBinary)
	return 0
}

//go:wasmexport response_ptr
func responsePtr() uint32 { return respPtr }

//go:wasmexport response_len
func responseLen() uint32 { return respLen }

//go:wasmexport response_is_binary
func responseIsBinary() uint32 { return respBinary }

func setResponse(data []byte, isBinary bool) {
	respBuf = data
	respPtr = bytesPtr(respBuf)
	respLen = uint32(len(respBuf))
	if isBinary {
		respBinary = 1
	} else {
		respBinary = 0
	}
}

// wasmConn implements api.Conn, delegating filesystem callbacks to the host and
// treating every other connection operation as a no-op (the module never runs a
// message loop or calls back into the client for anything but the FS).
type wasmConn struct{}

func (wasmConn) Run(ctx context.Context) error                               { return nil }
func (wasmConn) Notify(ctx context.Context, method string, params any) error { return nil }

func (wasmConn) Call(ctx context.Context, method string, params any) (json.Value, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	name := []byte(method)

	callMu.Lock()
	defer callMu.Unlock()

	result := hostCallback(bytesPtr(name), uint32(len(name)), bytesPtr(payload), uint32(len(payload)))
	// bytesPtr hands the host a raw address, which does not keep its referent
	// alive on its own; hold both slices until the host is done with them.
	runtime.KeepAlive(name)
	runtime.KeepAlive(payload)

	failed := result&hostErrorFlag != 0
	length := result &^ hostErrorFlag
	var res []byte
	if length > 0 {
		res = make([]byte, length)
		hostReadResult(bytesPtr(res), length)
		runtime.KeepAlive(res)
	}

	if failed {
		message := string(res)
		if message == "" {
			message = "unknown error"
		}
		return nil, fmt.Errorf("host callback %q failed: %s", method, message)
	}
	if length == 0 {
		return json.Value(nil), nil
	}
	return json.Value(res), nil
}

// readMem returns a view over length bytes of linear memory at ptr.
func readMem(ptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
}

// bytesPtr returns the linear-memory offset of b's backing array. The result is
// a bare address, so it must never outlive a runtime.KeepAlive of b: a uintptr
// does not keep its referent alive.
func bytesPtr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}
