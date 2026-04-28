package cmd

// Auto-discovery of a scan artifact for `gitmap reclone` when the
// user invoked the command with no <file> positional argument.
//
// Convention: `gitmap scan` always writes its artifacts under
// ./.gitmap/output/ relative to the scanned root. We treat the
// process CWD as that root and probe the canonical filenames in a
// fixed priority order (JSON first because it is the richest /
// least-lossy representation, CSV as a fallback for environments
// that only kept the spreadsheet view).
//
// We deliberately do NOT walk upward or scan sibling directories:
// silent path-magic across a tree would be surprising and would make
// "which manifest fed this reclone?" un-answerable from the command
// line alone. The MsgCloneNowAutoPickup stderr line documents the
// chosen path so the run is fully reproducible.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alimtvnetwork/gitmap-v7/gitmap/constants"
)

// resolveCloneNowSource picks the input file for `gitmap reclone`
// from three possible sources, in priority order:
//
//	1. --manifest <path>   (explicit, highest priority)
//	2. positional <file>   (legacy form, kept for back-compat)
//	3. auto-pickup         (./.gitmap/output/gitmap.{json,csv})
//
// Supplying BOTH --manifest AND a positional file is a usage error
// (exit 2): rather than silently preferring one, we refuse so the
// run is unambiguous and reproducible. When neither is supplied and
// auto-pickup also misses, exit 2 with the standard missing-arg
// message — same exit code, different cause, but the user fix is
// always one of: pass --manifest, pass a positional file, or run
// `gitmap scan` first to populate the conventional output dir.
func resolveCloneNowSource(fs *flag.FlagSet, manifest string) string {
	if manifest != "" && fs.NArg() >= 1 {
		fmt.Fprintf(os.Stderr, constants.MsgCloneNowManifestConflict,
			fs.Arg(0), manifest)
		os.Exit(2)
	}
	if manifest != "" {

		return manifest
	}
	if fs.NArg() >= 1 {

		return fs.Arg(0)
	}
	picked, ok := autoPickupRecloneManifest()
	if !ok {
		fmt.Fprintln(os.Stderr, constants.MsgCloneNowMissingArg)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, constants.MsgCloneNowAutoPickup, picked)

	return picked
}

// autoPickupRecloneManifest returns the first existing scan-artifact
// path under ./.gitmap/output/ (CWD-relative) and ok=true, or "",
// false if neither candidate file is present. Only regular files
// count -- a directory at the candidate path is treated as a miss
// rather than silently used.
func autoPickupRecloneManifest() (string, bool) {
	base := filepath.Join(constants.GitMapDir, constants.OutputDirName)
	candidates := []string{
		filepath.Join(base, constants.DefaultJSONFile),
		filepath.Join(base, constants.DefaultCSVFile),
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {

			return path, true
		}
	}

	return "", false
}
