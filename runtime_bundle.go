// Package runtimebundle embeds the same pinned Nix environment used by the
// prepared t3fleet images, for installing it on a Namespace fallback worker.
package runtimebundle

import (
	"embed"
	"fmt"
)

//go:embed flake.nix flake.lock nix/runtime.nix nix/t3-server.nix nix/runtime-tests.nix
var files embed.FS

// Sources returns the flake and its inputs in the order expected by the
// private Namespace install command.
func Sources() []string {
	names := [...]string{"flake.nix", "flake.lock", "nix/runtime.nix", "nix/t3-server.nix", "nix/runtime-tests.nix"}
	contents := make([]string, 0, len(names))
	for _, name := range names {
		data, err := files.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("missing embedded runtime file %s: %v", name, err))
		}
		contents = append(contents, string(data))
	}
	return contents
}
