package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/treehouse/v3/internal/config"
	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// resolvePoolDir is the one place a command turns a repository and its loaded
// config into the pool directory, so get, status, pool-dir and every other
// pool command honor the same --root, TREEHOUSE_ROOT, config and default.
func resolvePoolDir(repoRoot string, cfg config.Config) (string, error) {
	return config.ResolvePoolDir(repoRoot, config.ResolveRoot(rootFlag, cfg))
}

var poolDirCmd = &cobra.Command{
	Use:   "pool-dir",
	Short: "Print the pool directory 'treehouse get' would use here",
	Long: `Print the absolute pool directory that 'treehouse get' would allocate from in
the current repository, as one line on stdout.

It resolves the directory exactly as get and status do (--root, TREEHOUSE_ROOT,
the project treehouse.toml, the user-level config, then the default root), but
only computes it: it scans no slots, takes no pool lock, and writes nothing, so
it returns at once even while another command holds the pool.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		repoRoot, err := vcs.FindMainRepoRoot()
		if err != nil {
			return fmt.Errorf("not in a git or jj repository: %w", err)
		}

		cfg, err := config.Load(repoRoot)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		poolDir, err := resolvePoolDir(repoRoot, cfg)
		if err != nil {
			return err
		}

		fmt.Fprintln(os.Stdout, poolDir)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(poolDirCmd)
}
