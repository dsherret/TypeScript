package project

import (
	"context"
	"fmt"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

// TestAllowNonTsExtensionsIsForcedOnConfiguredProjects checks the session option reaches
// a configured project's parsed options: a config that names a file of a foreign
// extension holds it only when the host allows it, because the option has no tsconfig
// spelling of its own.
func TestAllowNonTsExtensionsIsForcedOnConfiguredProjects(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	const configFile = "/p/tsconfig.json"
	const textFile = "/p/a.txt"
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allowNonTsExtensions=%t", allow), func(t *testing.T) {
			t.Parallel()
			session := NewSession(&SessionInit{
				BackgroundCtx: context.Background(),
				Options: &SessionOptions{
					CurrentDirectory:     "/",
					DefaultLibraryPath:   bundled.LibPath(),
					PositionEncoding:     lsproto.PositionEncodingKindUTF8,
					AllowNonTsExtensions: allow,
				},
				FS: bundled.WrapFS(vfstest.FromMap(map[string]any{
					configFile: `{"files": ["a.txt"]}`,
					textFile:   "export const a = 1;",
				}, true /*useCaseSensitiveFileNames*/)),
				Client: addRootsClient{},
			})
			defer session.Close()

			openProjects := collections.NewSetWithSizeHint[string](1)
			openProjects.Add(configFile)
			snapshot, err := session.APIUpdate(context.Background(), FileChangeSummary{}, &APISnapshotRequest{OpenProjects: openProjects})
			assert.NilError(t, err)
			defer snapshot.Deref(session)

			project := snapshot.ProjectCollection.ConfiguredProject(tspath.Path(configFile))
			assert.Assert(t, project != nil)
			assert.Equal(t, project.CommandLine.CompilerOptions().AllowNonTsExtensions.IsTrue(), allow)
			assert.Equal(t, project.GetProgram().GetSourceFileByPath(tspath.Path(textFile)) != nil, allow)
		})
	}
}
