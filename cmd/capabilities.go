package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// capabilitiesSchema is the version of the capabilities object itself. Bump it
// only when an existing field changes meaning; adding a field does not need it.
const capabilitiesSchema = 1

// capabilitiesReport is the whole `treehouse capabilities --json` output. The
// field names are an interface other tools probe, so a test pins them.
type capabilitiesReport struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	// PersistentRecoveredLeases is true when this binary never frees,
	// moves, or reassigns a recovered pool entry or its worktree as a side
	// effect of another command: a recovered lease stays until a return
	// names its slot. It is a statement about this build's behavior, never
	// derived from the version string.
	PersistentRecoveredLeases bool `json:"persistent_recovered_leases"`
}

var capabilitiesJSON bool

var capabilitiesCmd = &cobra.Command{
	Use:   "capabilities",
	Short: "Print what this treehouse build guarantees, without touching any pool",
	Long: `Print the behaviors of this build that other tools may rely on.

With --json, print one JSON object:

  {"schema_version":1,"version":"<build version>","persistent_recovered_leases":true}

persistent_recovered_leases is true when no command frees, moves, or reassigns a
recovered pool entry or its worktree as a side effect of another command: a
recovered lease stays until a return names its slot. Treehouse builds without
this guarantee have no 'capabilities' command, so they fail with an unknown
command error. A tool must treat that failure, a nonzero exit, output that is
not one JSON object, and an absent or false persistent_recovered_leases the same
way: not proven.

The command resolves no pool, reads no repository, configuration, or state,
takes no lock, makes no network or update-check call, starts no background
process, and writes nothing, so it runs the same anywhere.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		report := capabilitiesReport{
			SchemaVersion:             capabilitiesSchema,
			Version:                   version,
			PersistentRecoveredLeases: true,
		}
		if !capabilitiesJSON {
			fmt.Fprintf(os.Stdout, "schema_version: %d\nversion: %s\npersistent_recovered_leases: %t\n", report.SchemaVersion, report.Version, report.PersistentRecoveredLeases)
			return nil
		}
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, string(data))
		return nil
	},
}

func init() {
	capabilitiesCmd.Flags().BoolVar(&capabilitiesJSON, "json", false, "Print the capabilities as one JSON object")
	rootCmd.AddCommand(capabilitiesCmd)
}
