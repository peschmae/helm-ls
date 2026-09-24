package cmds

import (
	"os"

	"github.com/mrjosh/helm-ls/internal/plato"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start Plato template language server",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			plato.Serve(stdrwc{})
		},
	}

	return cmd
}

type stdrwc struct{}

func (stdrwc) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (stdrwc) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (stdrwc) Close() error {
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	return os.Stdout.Close()
}
