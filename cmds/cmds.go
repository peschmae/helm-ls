package cmds

import (
	"github.com/mrjosh/helm-ls/internal/version"
	"github.com/spf13/cobra"
)

var versionInfo *version.BuildInfo

func Start(vi *version.BuildInfo, rootCmd *cobra.Command) error {
	if vi.Version == "" {
		vi.Version = "development"
	}
	vi.BuildType = "Release"
	if vi.Version == "development" {
		vi.BuildType = "Development"
	} else if vi.Branch == "master" {
		vi.BuildType = "Nightly"
	}
	versionInfo = vi
	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newServeCmd())
	return rootCmd.Execute()
}
