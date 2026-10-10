package testutil

import "fmt"

// NpmCIFailAlways fails every `npm ci`, install and --dry-run probe alike:
// package.json and the lockfile are out of sync, so the pipeline re-resolves.
func NpmCIFailAlways([]string) error {
	return fmt.Errorf("exit status 1 (fake npm ci failure)")
}

// NpmCIFailDryRunPasses fails the install but lets the --dry-run probe
// succeed: npm ci broke for a reason other than a desync (say, a network
// error), and the pipeline must fail instead of silently re-resolving.
func NpmCIFailDryRunPasses(args []string) error {
	for _, a := range args {
		if a == "--dry-run" {
			return nil
		}
	}
	return fmt.Errorf("exit status 1 (fake npm ci failure)")
}
