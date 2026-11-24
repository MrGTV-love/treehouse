package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/treehouse/v3/internal/config"
	"github.com/kunchenguid/treehouse/v3/internal/pool"
	"github.com/kunchenguid/treehouse/v3/internal/ui"
	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

var (
	pruneYes     bool
	pruneAll     bool
	pruneGlobal  bool
	pruneOrphans bool
	pruneVerbose bool
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove stale worktrees and opted-in orphans from the pool",
	Long: `Remove stale worktrees and opted-in orphans from the pool to reclaim disk space.

A worktree is stale only when treehouse manages it, no owner reservation or
running process is using it, it has no uncommitted changes, and its HEAD is
already merged into the default branch. Git slots also refuse ignored files
unless each is an authenticated seed matching what current seeding would write:
regular-file bytes, including link-target text for a symbolic source. The source
must lie outside the slot and pool after resolving parent links and must not
share the slot file's inode; a symbolic-link source whose target lies in the
slot or the pool is not independent; mode alone is not compared.
This is checked during planning and again
before deletion; concurrent writes after the last check are not protected.
Git slots with replacement refs, locked or modules metadata, or an initialized
submodule are also skipped, as foreign-slot reclamation skips them; a slot refused
for an ignored path is listed under "unsafe ignored files" with the path shown.
A failure to run the check itself keeps the slot and is reported as "cannot
verify worktree", not as "unsafe ignored files".
The ignored-file judgment does not cover jj slots. A Git orphan included by
--prune-orphans is refused, with every path named, when it holds any file other
than its .git marker: with the index and the repository's excludes gone, an
ignored file cannot be told from an untracked or committed one.

Prune is a dry run by default. Pass --yes to delete the listed candidates.
Backing-repository-missing orphans are reported by default. Pass --prune-orphans
to include them as unverified candidates, and combine it with --yes to delete
them. A Git orphan that holds anything but its .git marker is kept and listed
with its paths (the first 10 without --verbose).
Pass --all or --global to sweep every managed pool under the user-level
treehouse root from any directory. Global prune derives each worktree's owning
repository from git metadata and requires the configured root to be unset or
absolute.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if pruneAll || pruneGlobal {
			cfg, err := config.LoadGlobal()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			poolRoot, err := config.ResolvePoolRoot("", config.ResolveRoot(rootFlag, cfg))
			if err != nil {
				return err
			}

			result, err := pool.PruneAllWithOptions(poolRoot, pool.PruneOptions{
				DryRun:       !pruneYes,
				PruneOrphans: pruneOrphans,
				PreDestroy:   cfg.Hooks.PreDestroy,
			})
			if err != nil {
				return err
			}

			printPruneAllResult(os.Stdout, result, !pruneYes, pruneOrphans, pruneVerbose)
			return nil
		}

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

		result, err := pool.PruneWithOptions(repoRoot, poolDir, pool.PruneOptions{
			DryRun:       !pruneYes,
			PruneOrphans: pruneOrphans,
			PreDestroy:   cfg.Hooks.PreDestroy,
		})
		if err != nil {
			return err
		}

		printPruneResult(os.Stdout, result, !pruneYes, pruneOrphans, pruneVerbose)
		return nil
	},
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneYes, "yes", false, "Delete listed prune candidates instead of doing a dry run")
	pruneCmd.Flags().BoolVar(&pruneAll, "all", false, "Sweep every managed pool under the user-level treehouse root")
	pruneCmd.Flags().BoolVar(&pruneGlobal, "global", false, "Alias for --all")
	pruneCmd.Flags().BoolVar(&pruneOrphans, "prune-orphans", false, "Include backing-repository-missing orphans in prune candidates; a Git orphan holding anything but its .git marker is kept and listed with its paths")
	pruneCmd.Flags().BoolVarP(&pruneVerbose, "verbose", "v", false, "Show detailed skip diagnostics")
	rootCmd.AddCommand(pruneCmd)
}

func printPruneResult(w io.Writer, result pool.PruneResult, dryRun bool, pruneOrphans bool, verbose bool) {
	if dryRun {
		if len(result.Candidates) == 0 {
			fmt.Fprintln(w, "🌳 No stale worktrees to prune.")
			printPruneSkipped(w, result.Skipped, verbose)
			printPruneOrphanHint(w, result.Skipped, false, pruneOrphans, false)
			return
		}

		fmt.Fprintf(w, "🌳 Dry run: would prune %d %s %s and reclaim %s.\n",
			len(result.Candidates),
			pruneCandidateKind(result.Candidates),
			plural("worktree", len(result.Candidates)),
			formatBytes(result.ReclaimableBytes),
		)
		printPruneWorktrees(w, result.Candidates)
		printPruneSkipped(w, result.Skipped, verbose)
		fmt.Fprintf(w, "🌳 Re-run with %s to delete these worktrees.\n", pruneDeleteFlags(false, pruneOrphans))
		printPruneOrphanHint(w, result.Skipped, false, pruneOrphans, false)
		return
	}

	if len(result.Pruned) == 0 {
		fmt.Fprintln(w, "🌳 No stale worktrees pruned.")
		printPruneSkipped(w, result.Skipped, verbose)
		printPruneOrphanHint(w, result.Skipped, false, pruneOrphans, true)
		return
	}

	fmt.Fprintf(w, "🌳 Pruned %d %s %s and freed %s.\n",
		len(result.Pruned),
		pruneCandidateKind(result.Pruned),
		plural("worktree", len(result.Pruned)),
		formatBytes(result.FreedBytes),
	)
	printPruneWorktrees(w, result.Pruned)
	printPruneSkipped(w, result.Skipped, verbose)
	printPruneOrphanHint(w, result.Skipped, false, pruneOrphans, true)
}

func printPruneAllResult(w io.Writer, result pool.PruneAllResult, dryRun bool, pruneOrphans bool, verbose bool) {
	poolCount := len(result.Pools)
	if dryRun {
		if len(result.Result.Candidates) == 0 {
			fmt.Fprintf(w, "🌳 No stale worktrees to prune across %d %s.\n", poolCount, plural("pool", poolCount))
			printPrunePools(w, result.Pools, true)
			printPruneSkipped(w, result.Result.Skipped, verbose)
			printPruneOrphanHint(w, result.Result.Skipped, true, pruneOrphans, false)
			return
		}

		fmt.Fprintf(w, "🌳 Dry run: would prune %d %s %s across %d %s and reclaim %s.\n",
			len(result.Result.Candidates),
			pruneCandidateKind(result.Result.Candidates),
			plural("worktree", len(result.Result.Candidates)),
			poolCount,
			plural("pool", poolCount),
			formatBytes(result.Result.ReclaimableBytes),
		)
		printPrunePools(w, result.Pools, true)
		printPruneWorktrees(w, result.Result.Candidates)
		printPruneSkipped(w, result.Result.Skipped, verbose)
		fmt.Fprintf(w, "🌳 Re-run with %s to delete these worktrees.\n", pruneDeleteFlags(true, pruneOrphans))
		printPruneOrphanHint(w, result.Result.Skipped, true, pruneOrphans, false)
		return
	}

	if len(result.Result.Pruned) == 0 {
		fmt.Fprintf(w, "🌳 No stale worktrees pruned across %d %s.\n", poolCount, plural("pool", poolCount))
		printPrunePools(w, result.Pools, false)
		printPruneSkipped(w, result.Result.Skipped, verbose)
		printPruneOrphanHint(w, result.Result.Skipped, true, pruneOrphans, true)
		return
	}

	fmt.Fprintf(w, "🌳 Pruned %d %s %s across %d %s and freed %s.\n",
		len(result.Result.Pruned),
		pruneCandidateKind(result.Result.Pruned),
		plural("worktree", len(result.Result.Pruned)),
		poolCount,
		plural("pool", poolCount),
		formatBytes(result.Result.FreedBytes),
	)
	printPrunePools(w, result.Pools, false)
	printPruneWorktrees(w, result.Result.Pruned)
	printPruneSkipped(w, result.Result.Skipped, verbose)
	printPruneOrphanHint(w, result.Result.Skipped, true, pruneOrphans, true)
}

// printPrunePools names every pool a global prune discovered, including pools
// with nothing to prune (for example one holding only leased slots), so a pool
// can never be silently absent from the report. It changes no eligibility.
func printPrunePools(w io.Writer, pools []pool.PrunePoolResult, dryRun bool) {
	if len(pools) == 0 {
		return
	}
	fmt.Fprintf(w, "🌳 Inspected %d %s:\n", len(pools), plural("pool", len(pools)))
	for _, p := range pools {
		acted := len(p.Result.Pruned)
		verb := "pruned"
		if dryRun {
			acted = len(p.Result.Candidates)
			verb = "would prune"
		}
		fmt.Fprintf(w, "  %s  %s %d, skipped %d\n", ui.PrettyPath(p.PoolDir), verb, acted, len(p.Result.Skipped))
	}
}

func printPruneWorktrees(w io.Writer, worktrees []pool.PruneWorktree) {
	sizeWidth := 0
	sizes := make([]string, len(worktrees))
	for i, wt := range worktrees {
		sizes[i] = formatBytes(wt.Bytes)
		if len(sizes[i]) > sizeWidth {
			sizeWidth = len(sizes[i])
		}
	}

	for i, wt := range worktrees {
		fmt.Fprintf(w, "%-4s  %*s  %s", wt.Name, sizeWidth, sizes[i], ui.PrettyPath(wt.Path))
		if wt.Warning != "" {
			fmt.Fprintf(w, "  %s", wt.Warning)
		}
		fmt.Fprintln(w)
	}
}

func printPruneSkipped(w io.Writer, skipped []pool.PruneSkipped, verbose bool) {
	if len(skipped) == 0 {
		return
	}

	fmt.Fprintf(w, "🌳 Skipped %d unsafe idle %s:\n", len(skipped), plural("worktree", len(skipped)))
	for _, category := range orderedPruneSkipCategories(skipped) {
		group := pruneSkipGroup(skipped, category)
		fmt.Fprintf(w, "  %s:\n", escapePruneDetail(category))
		reasonWidth := 0
		for _, wt := range group {
			if reason := escapePruneDetail(wt.Reason); pruneSkipShowsReason(wt) && len(reason) > reasonWidth {
				reasonWidth = len(reason)
			}
		}
		for _, wt := range group {
			if pruneSkipShowsReason(wt) {
				fmt.Fprintf(w, "  %-4s  %-*s  %s\n", wt.Name, reasonWidth, escapePruneDetail(wt.Reason), ui.PrettyPath(wt.Path))
			} else {
				fmt.Fprintf(w, "  %-4s  %s\n", wt.Name, ui.PrettyPath(wt.Path))
			}
			if wt.Detail != "" && (verbose || pruneSkipCategory(wt) == pool.PruneSkipIgnoredFiles) {
				printPruneSkipDetail(w, wt, verbose)
			}
		}
	}
}

const prunePathListLimit = 10

func printPruneSkipDetail(w io.Writer, wt pool.PruneSkipped, verbose bool) {
	if len(wt.Paths) == 0 {
		fmt.Fprintf(w, "        detail: %s\n", escapePruneDetail(wt.Detail))
		return
	}
	shown := wt.Paths
	if !verbose && len(shown) > prunePathListLimit {
		shown = shown[:prunePathListLimit]
	}
	escaped := make([]string, len(shown))
	for i, path := range shown {
		escaped[i] = escapePruneDetail(path)
	}
	fmt.Fprintf(w, "        detail: %s\n", strings.Join(escaped, ", "))
	if remaining := len(wt.Paths) - len(shown); remaining > 0 {
		fmt.Fprintf(w, "        and %d more (use --verbose to list all)\n", remaining)
	}
}

// escapePruneDetail keeps skip text that can hold checkout path names on one
// line and stops it from sending terminal control sequences.
func escapePruneDetail(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

func pruneSkipShowsReason(wt pool.PruneSkipped) bool {
	return wt.Reason != "" && wt.Reason != pruneSkipCategory(wt)
}

func pruneSkipCategory(wt pool.PruneSkipped) string {
	if wt.Category != "" {
		return wt.Category
	}
	if wt.Reason != "" {
		return wt.Reason
	}
	return "cannot verify worktree"
}

func orderedPruneSkipCategories(skipped []pool.PruneSkipped) []string {
	groups := make(map[string]struct{})
	for _, wt := range skipped {
		groups[pruneSkipCategory(wt)] = struct{}{}
	}

	preferred := []string{
		pool.PruneSkipUncommitted,
		pool.PruneSkipUnmerged,
		pool.PruneSkipOrphanedBackingRepo,
		pool.PruneSkipOriginUnreachable,
	}
	var ordered []string
	for _, category := range preferred {
		if _, ok := groups[category]; ok {
			ordered = append(ordered, category)
			delete(groups, category)
		}
	}
	var remaining []string
	for category := range groups {
		remaining = append(remaining, category)
	}
	sort.Strings(remaining)
	return append(ordered, remaining...)
}

func pruneSkipGroup(skipped []pool.PruneSkipped, category string) []pool.PruneSkipped {
	var group []pool.PruneSkipped
	for _, wt := range skipped {
		if pruneSkipCategory(wt) == category {
			group = append(group, wt)
		}
	}
	return group
}

func pruneCandidateKind(worktrees []pool.PruneWorktree) string {
	for _, wt := range worktrees {
		if wt.Orphaned {
			return "stale/orphaned"
		}
	}
	return "stale"
}

func pruneDeleteFlags(all bool, pruneOrphans bool) string {
	var flags []string
	if all {
		flags = append(flags, "--all")
	}
	if pruneOrphans {
		flags = append(flags, "--prune-orphans")
	}
	flags = append(flags, "--yes")
	return strings.Join(flags, " ")
}

func printPruneOrphanHint(w io.Writer, skipped []pool.PruneSkipped, all bool, pruneOrphans bool, yes bool) {
	if pruneOrphans || !hasSkippedCategory(skipped, pool.PruneSkipOrphanedBackingRepo) {
		return
	}
	if yes {
		fmt.Fprintf(w, "🌳 Re-run with %s to delete true orphans whose backing repository is missing; a Git orphan holding anything but its .git marker is kept and listed with its paths.\n", pruneDeleteFlags(all, true))
		return
	}
	flags := "--prune-orphans"
	if all {
		flags = "--all --prune-orphans"
	}
	fmt.Fprintf(w, "🌳 Re-run with %s to include true orphans in the dry run; add --yes to delete them. A Git orphan holding anything but its .git marker is kept and listed with its paths.\n", flags)
}

func hasSkippedCategory(skipped []pool.PruneSkipped, category string) bool {
	for _, wt := range skipped {
		if pruneSkipCategory(wt) == category {
			return true
		}
	}
	return false
}

func plural(word string, count int) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(bytes)
	unit := "B"
	for _, next := range units {
		value /= 1024
		unit = next
		if value < 1024 {
			break
		}
	}

	formatted := fmt.Sprintf("%.1f", value)
	formatted = strings.TrimSuffix(strings.TrimSuffix(formatted, "0"), ".")
	return formatted + " " + unit
}
