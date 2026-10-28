package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/treehouse/v3/internal/config"
	"github.com/kunchenguid/treehouse/v3/internal/pool"
	"github.com/kunchenguid/treehouse/v3/internal/ui"
	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

var (
	leaseHolderFlag string
	leaseJSONFlag   bool
	leasePathFlag   string
)

var leaseCmd = &cobra.Command{
	Use:   "lease <name> | lease --path <slot path>",
	Short: "Durably lease an existing pool worktree in place, without touching it",
	Long: `Mark an existing pool worktree, identified by its name (the number shown
by 'treehouse status'), as durably leased using the same persistent lease
state 'treehouse get --lease' writes.

A name is resolved against the pool of the repository you stand in, so it is
ambiguous when two pools share a slot number. Pass --path with the exact
worktree root instead to lease that one worktree: its pool is found from the
path itself, exactly as 'treehouse return <path>' does, from any directory.
The name and --path forms are mutually exclusive.

This is a state-only operation: lease never resets, fetches, cleans, or
checks out the worktree, so it is safe on a slot that already holds live
work. Use it to give a worktree acquired with plain 'get' - or created
before leases existed - the same protection a leased acquisition has: a
leased worktree is never handed out by a later get and never removed by
prune or destroy, even with no process running inside it, until you release
it with 'treehouse return <path>'.

By default it prints only the worktree's absolute path to stdout; add --json
for the lease identity and metadata. All banners go to stderr.`,
	Args: validateLeaseArgs,
	RunE: leaseRunE,
}

func init() {
	leaseCmd.Flags().StringVar(&leaseHolderFlag, "lease-holder", "", "Optional label recorded as the lease holder (defaults to $TREEHOUSE_LEASE_HOLDER)")
	leaseCmd.Flags().BoolVar(&leaseJSONFlag, "json", false, "Print the lease as JSON")
	leaseCmd.Flags().StringVar(&leasePathFlag, "path", "", "Lease the worktree at this exact path instead of naming it; its pool is found from the path")
	rootCmd.AddCommand(leaseCmd)
}

func validateLeaseArgs(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed("path") {
		if len(args) > 0 {
			return fmt.Errorf("lease takes either a worktree name or --path, not both")
		}
		if leasePathFlag == "" {
			return fmt.Errorf("--path requires a worktree path")
		}
		return nil
	}
	return cobra.ExactArgs(1)(cmd, args)
}

// resolveLeasePathTarget returns the pool and slot name for --path. The path
// finds its own pool through resolveReturnPath, the resolver 'return <path>'
// uses, so the two verbs agree on what a managed worktree is.
func resolveLeasePathTarget(path string) (poolDir, name string, err error) {
	wtPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	target, err := resolveReturnPath(wtPath, true)
	if err != nil {
		if errors.Is(err, errReturnWorktreeUnmanaged) {
			return "", "", fmt.Errorf("worktree %s is not managed by treehouse", wtPath)
		}
		return "", "", err
	}
	entry, err := pool.FindByPath(target.poolDir, target.path)
	if err != nil {
		return "", "", err
	}
	if entry == nil {
		return "", "", fmt.Errorf("worktree %s is not managed by treehouse", wtPath)
	}
	return target.poolDir, entry.Name, nil
}

func leaseRunE(cmd *cobra.Command, args []string) error {
	holder := leaseHolderFlag
	if holder == "" {
		holder = os.Getenv("TREEHOUSE_LEASE_HOLDER")
	}

	var poolDir, name string
	if cmd.Flags().Changed("path") {
		var err error
		poolDir, name, err = resolveLeasePathTarget(leasePathFlag)
		if err != nil {
			return err
		}
	} else {
		repoRoot, err := vcs.FindMainRepoRoot()
		if err != nil {
			return fmt.Errorf("not in a git or jj repository: %w", err)
		}

		cfg, err := config.Load(repoRoot)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		poolDir, err = resolvePoolDir(repoRoot, cfg)
		if err != nil {
			return fmt.Errorf("failed to resolve pool directory: %w", err)
		}
		name = args[0]
	}

	lease, err := pool.LeaseExisting(poolDir, name, holder)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "🌳 Leased worktree %s at %s. Run 'treehouse return %s' to release it.\n",
		name, ui.PrettyPath(lease.Path), ui.PrettyPath(lease.Path))
	if leaseJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(lease)
	}
	// The bare path is the only thing on stdout, so callers can capture it.
	fmt.Fprintln(os.Stdout, lease.Path)
	return nil
}
