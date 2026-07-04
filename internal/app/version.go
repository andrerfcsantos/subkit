package app

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func versionCommand() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "subkit %s\n", version)
			fmt.Fprintf(cmd.OutOrStdout(), "generated %s\n", humanBuildDate(date))
			if verbose {
				fmt.Fprintf(cmd.OutOrStdout(), "commit %s\n", commit)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show commit information")
	return cmd
}

func humanBuildDate(value string) string {
	if value == "" {
		return "unknown"
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("January 2, 2006 at 15:04 MST")
}
