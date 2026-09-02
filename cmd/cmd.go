// Package cmd implements the gem-usage-lens CLI dispatch.
//
// Dispatch uses the standard library `flag` package (no third-party CLI
// framework) to keep the tool dependency-light and offline-buildable.
package cmd

import (
	"fmt"
	"io"
	"os"
)

// Execute runs the CLI. version is injected from main via -ldflags.
func Execute(version string) {
	os.Exit(run(os.Args[1:], version, os.Stdout, os.Stderr))
}

// run is Execute without the process exit, so the dispatch is testable.
func run(args []string, version string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}

	rest := args[1:]
	var err error
	switch args[0] {
	case "ingest":
		err = runIngest(rest)
	case "reprice":
		err = runReprice(rest)
	case "report":
		err = runReport(rest)
	case "budget":
		err = runBudget(rest)
	case "sessions":
		err = runSessions(rest)
	case "models":
		err = runModels(rest)
	case "verify":
		err = runVerify(rest)
	case "doctor":
		err = runDoctor(rest)
	case "watch":
		err = runWatch(rest)
	case "daemon":
		err = runDaemon(rest)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, versionLine(version))
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		usage(stderr)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// versionLine is what both `version` and `--version` print — identical, so
// a Homebrew formula test that greps `--version` and a human running
// `version` see the same string.
func versionLine(version string) string {
	return "gem-usage-lens " + version
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gem-usage-lens — collect token usage & cost from gem-agent session transcripts

Usage:
  gem-usage-lens <command> [flags]

Commands:
  ingest     Incrementally load new/appended transcript records into the durable store
  reprice    Recompute stored costs after a rate-table change
  report     Aggregate stored usage by day / session / project / model / source
  budget     Show the calendar-month budget state (used, remaining, pace)
  sessions   List sessions with tokens and cost
  models     Show the rate table (with its verification date) and config overrides
  verify     Check transcript accounting (prompt + output + thoughts == total) and report partial files
  doctor     Diagnose the resolved sessions root / store / config paths
  watch      Poll and ingest continuously, printing live cost deltas
  daemon     Install/uninstall/status a periodic-ingest service (macOS launchd)
  version    Print the version

Run 'gem-usage-lens <command> -h' for command-specific flags.

Note: costs are a Vertex AI list-price EQUIVALENT (notional), not an actual bill.
`)
}
