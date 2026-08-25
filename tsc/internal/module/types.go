package module

import (
	"fmt"
	"math/bits"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs"
)

type ResolutionHost interface {
	FS() vfs.FS
	GetCurrentDirectory() string
}

// ModuleNameResolutionHook is implemented by a ResolutionHost that wants to
// answer module resolutions itself.
//
// It exists for hosts that resolve by rules the compiler does not implement —
// a Deno-style host that writes `./mod.ts` where node would write `./mod`, or
// anything else that maps a specifier to a file its own way. The compiler asks
// before doing its own work and takes the answer as final, so a host that
// returns handled must return a resolution it is happy to be believed about.
//
// Returning handled = false is not an error: it means the host has no opinion
// about this specifier and the compiler should resolve it normally. A host that
// only rewrites some specifiers therefore says nothing about the rest.
type ModuleNameResolutionHook interface {
	ResolveModuleNameFromHost(moduleName string, containingFile string, resolutionMode core.ResolutionMode) (answer *HostModuleResolution, handled bool)
}

// HostModuleResolution is what a ModuleNameResolutionHook answers with.
//
// A host may either resolve the specifier itself, by setting Resolved, or hand
// back a different specifier for the compiler to resolve, by setting ModuleName.
// The second is what a host that only rewrites wants: a Deno-style host turning
// `./mod.ts` into `./mod` is saying where to look, not how to look, and would
// otherwise have to reimplement node resolution to say so.
//
// Setting neither means the specifier resolves to nothing.
type HostModuleResolution struct {
	Resolved   *ResolvedModule
	ModuleName string
}

type ModeAwareCacheKey struct {
	Name string
	Mode core.ResolutionMode
}

type ResolvedProjectReference interface {
	ConfigName() string
	CompilerOptions() *core.CompilerOptions
}

type NodeResolutionFeatures int32

const (
	NodeResolutionFeaturesImports NodeResolutionFeatures = 1 << iota
	NodeResolutionFeaturesSelfName
	NodeResolutionFeaturesExports
	NodeResolutionFeaturesExportsPatternTrailers
	// allowing `#/` root imports in package.json imports field
	// not supported until mass adoption - https://github.com/nodejs/node/pull/60864
	NodeResolutionFeaturesImportsPatternRoot

	NodeResolutionFeaturesNone            NodeResolutionFeatures = 0
	NodeResolutionFeaturesAll                                    = NodeResolutionFeaturesImports | NodeResolutionFeaturesSelfName | NodeResolutionFeaturesExports | NodeResolutionFeaturesExportsPatternTrailers | NodeResolutionFeaturesImportsPatternRoot
	NodeResolutionFeaturesNode16Default                          = NodeResolutionFeaturesImports | NodeResolutionFeaturesSelfName | NodeResolutionFeaturesExports | NodeResolutionFeaturesExportsPatternTrailers
	NodeResolutionFeaturesNodeNextDefault                        = NodeResolutionFeaturesAll
	NodeResolutionFeaturesBundlerDefault                         = NodeResolutionFeaturesImports | NodeResolutionFeaturesSelfName | NodeResolutionFeaturesExports | NodeResolutionFeaturesExportsPatternTrailers | NodeResolutionFeaturesImportsPatternRoot
)

type PackageId struct {
	Name             string
	SubModuleName    string
	Version          string
	PeerDependencies string
}

func (p *PackageId) String() string {
	return fmt.Sprintf("%s@%s%s", p.PackageName(), p.Version, p.PeerDependencies)
}

func (p *PackageId) PackageName() string {
	if p.SubModuleName != "" {
		return p.Name + "/" + p.SubModuleName
	}
	return p.Name
}

type ResolvedModule struct {
	ResolutionDiagnostics        []*ast.Diagnostic
	ResolvedFileName             string
	OriginalPath                 string
	Extension                    string
	ResolvedUsingTsExtension     bool
	ResolvedUsingExtraExtensions bool
	PackageId                    PackageId
	IsExternalLibraryImport      bool
	AlternateResult              string
}

func (r *ResolvedModule) IsResolved() bool {
	return r != nil && r.ResolvedFileName != ""
}

type ResolvedTypeReferenceDirective struct {
	ResolutionDiagnostics   []*ast.Diagnostic
	Primary                 bool
	ResolvedFileName        string
	OriginalPath            string
	PackageId               PackageId
	IsExternalLibraryImport bool
}

func (r *ResolvedTypeReferenceDirective) IsResolved() bool {
	return r.ResolvedFileName != ""
}

type extensions int32

const (
	extensionsTypeScript extensions = 1 << iota
	extensionsJavaScript
	extensionsDeclaration
	extensionsJson

	extensionsImplementationFiles = extensionsTypeScript | extensionsJavaScript
)

func (e extensions) String() string {
	result := make([]string, 0, bits.OnesCount(uint(e)))
	if e&extensionsTypeScript != 0 {
		result = append(result, "TypeScript")
	}
	if e&extensionsJavaScript != 0 {
		result = append(result, "JavaScript")
	}
	if e&extensionsDeclaration != 0 {
		result = append(result, "Declaration")
	}
	if e&extensionsJson != 0 {
		result = append(result, "JSON")
	}
	return strings.Join(result, ", ")
}

func (e extensions) Array() []string {
	result := []string{}
	if e&extensionsTypeScript != 0 {
		result = append(result, tspath.SupportedTSImplementationExtensions...)
	}
	if e&extensionsJavaScript != 0 {
		result = append(result, tspath.SupportedJSExtensionsFlat...)
	}
	if e&extensionsDeclaration != 0 {
		result = append(result, tspath.SupportedDeclarationExtensions...)
	}
	if e&extensionsJson != 0 {
		result = append(result, tspath.ExtensionJson)
	}
	return result
}

// GetResolved returns the resolution the host gave, as a result the compiler's
// callers can read. A host that resolved to nothing still owes them a struct:
// the compiler's own resolution never returns nil, and they read fields off what
// they are handed rather than checking first.
func (a *HostModuleResolution) GetResolved() *ResolvedModule {
	if a == nil || a.Resolved == nil {
		return &ResolvedModule{}
	}
	return a.Resolved
}
