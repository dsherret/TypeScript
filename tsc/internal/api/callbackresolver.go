package api

import (
	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"context"
	"fmt"

	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/module"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

// callbackResolveModuleName is the callback name a client enables to resolve
// module specifiers itself. Unlike the others it is not a filesystem operation,
// so it does not go through callbackFS.
const callbackResolveModuleName = "resolveModuleName"

// resolveModuleNameRequest is what the client is asked.
type resolveModuleNameRequest struct {
	ModuleName     string `json:"moduleName"`
	ContainingFile string `json:"containingFile"`
	// ResolutionMode is a core.ResolutionMode: 0 when the compiler has no opinion,
	// otherwise ModuleKindCommonJS or ModuleKindESNext.
	ResolutionMode core.ResolutionMode `json:"resolutionMode"`
}

// resolveModuleNameResponse is what the client may answer.
//
// The distinction the wire has to carry is three-way, so a bare file name will
// not do: the client may resolve the specifier, may say it resolves to nothing,
// or may decline to have an opinion and leave it to the compiler. A null
// response is declining; `{"resolved": null}` is resolving to nothing.
type resolveModuleNameResponse struct {
	Resolved *resolvedModuleResponse `json:"resolved"`
	// ModuleName asks the compiler to resolve a different specifier instead. It
	// takes precedence over Resolved, which a client sending both did not mean.
	ModuleName string `json:"moduleName,omitempty"`
}

type resolvedModuleResponse struct {
	ResolvedFileName string `json:"resolvedFileName"`
	// Extension of the resolved file. Derived from the file name when omitted,
	// which is what a client that only rewrites specifiers will want.
	Extension                string `json:"extension,omitempty"`
	IsExternalLibraryImport  bool   `json:"isExternalLibraryImport,omitempty"`
	ResolvedUsingTsExtension bool   `json:"resolvedUsingTsExtension,omitempty"`
}

// newCallbackModuleResolver returns the resolver a session with the
// resolveModuleName callback enabled hands to the compiler, or nil when the
// client did not enable it.
func newCallbackModuleResolver(ctx context.Context, conn ipc.Conn, enabled bool) func(string, string, core.ResolutionMode) (*module.HostModuleResolution, bool) {
	if !enabled {
		return nil
	}
	if conn == nil {
		panic("the resolveModuleName callback needs a connection to call")
	}

	return func(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (*module.HostModuleResolution, bool) {
		raw, err := conn.Call(ctx, callbackResolveModuleName, &resolveModuleNameRequest{
			ModuleName:     moduleName,
			ContainingFile: containingFile,
			ResolutionMode: resolutionMode,
		})
		if err != nil {
			panic(fmt.Errorf("resolveModuleName callback failed for %q from %q: %w", moduleName, containingFile, err))
		}
		if len(raw) == 0 || string(raw) == "null" {
			return nil, false
		}

		var response resolveModuleNameResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			panic(fmt.Errorf("resolveModuleName callback returned an undecodable answer for %q: %w", moduleName, err))
		}
		if response.ModuleName != "" {
			return &module.HostModuleResolution{ModuleName: response.ModuleName}, true
		}
		if response.Resolved == nil {
			// resolved to nothing, and the compiler should not try to do better
			return &module.HostModuleResolution{}, true
		}
		if response.Resolved.ResolvedFileName == "" {
			panic(fmt.Errorf("resolveModuleName callback resolved %q to an empty file name", moduleName))
		}

		extension := response.Resolved.Extension
		if extension == "" {
			extension = tspath.TryGetExtensionFromPath(response.Resolved.ResolvedFileName)
		}
		return &module.HostModuleResolution{Resolved: &module.ResolvedModule{
			ResolvedFileName:         response.Resolved.ResolvedFileName,
			Extension:                extension,
			IsExternalLibraryImport:  response.Resolved.IsExternalLibraryImport,
			ResolvedUsingTsExtension: response.Resolved.ResolvedUsingTsExtension,
		}}, true
	}
}
