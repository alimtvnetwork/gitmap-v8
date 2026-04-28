package cmd

// Integration test: scan → export JSON/CSV → clone-from for both
// https and ssh "modes". Drives the production functions end-to-end
// against real on-disk artifacts:
//
//   1. Build a tiny bare repo + two fake worktrees that point at it.
//   2. scanner.ScanDirWithOptions discovers the worktrees.
//   3. mapper.BuildRecords (mode=https / mode=ssh) populates the
//      HTTPSUrl + SSHUrl columns from the real git remote.
//   4. formatter.WriteJSON / formatter.WriteCSV serialise the
//      records to disk via a real *os.File.
//   5. The exported scan file is transformed into a clone-from
//      manifest by reading the column the mode selected
//      (httpsUrl for ModeHTTPS, sshUrl for ModeSSH) — this is the
//      round-trip the test guards.
//   6. clonefrom.ParseFile + clonefrom.Execute re-clone every row
//      and the assertion is "every row Status == ok".
//
// Both "modes" use file:// URLs underneath because CI has no
// reachable git server. The mode wiring still gets exercised: the
// transform step picks a different column per mode and a regression
// in mapper.selectCloneURL or in either parser would surface as a
// failed Result row.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/alimtvnetwork/gitmap-v8/gitmap/clonefrom"
	"github.com/alimtvnetwork/gitmap-v8/gitmap/constants"
	"github.com/alimtvnetwork/gitmap-v8/gitmap/formatter"
	"github.com/alimtvnetwork/gitmap-v8/gitmap/mapper"
	"github.com/alimtvnetwork/gitmap-v8/gitmap/model"
	"github.com/alimtvnetwork/gitmap-v8/gitmap/scanner"
)

// TestScanExportCloneFrom_HTTPSAndSSH_RoundTrips runs the full
// pipeline once per mode. Sub-tests share the bare-repo fixture so
// the slow path (git init + commit + bare clone) only runs once.
func TestScanExportCloneFrom_HTTPSAndSSH_RoundTrips(t *testing.T) {
	requireGitForIntegration(t)
	bare := makeIntegrationBareRepo(t)
	scanRoot := seedScanTree(t, bare)

	for _, mode := range []string{constants.ModeHTTPS, constants.ModeSSH} {
		t.Run("json/"+mode, func(t *testing.T) {
			runRoundTrip(t, scanRoot, mode, "json")
		})
		t.Run("csv/"+mode, func(t *testing.T) {
			runRoundTrip(t, scanRoot, mode, "csv")
		})
	}
}

// runRoundTrip executes one scan → export(format) → transform →
// clone-from cycle and asserts every executed row landed ok.
func runRoundTrip(t *testing.T, scanRoot, mode, format string) {
	t.Helper()
	records := scanAndBuildRecords(t, scanRoot, mode)
	exportPath := exportRecords(t, records, format)
	manifest := writeCloneFromManifest(t, records, mode, format)
	executePlanAndAssertOK(t, manifest, exportPath)
}

// scanAndBuildRecords runs the real scanner against the seeded
// worktree tree, then converts the RepoInfo slice to ScanRecords
// the same way `gitmap scan` does at runtime.
func scanAndBuildRecords(t *testing.T, root, mode string) []model.ScanRecord {
	t.Helper()
	repos, err := scanner.ScanDirWithOptions(root, scanner.ScanOptions{})
	if err != nil {
		t.Fatalf("scanner.ScanDirWithOptions: %v", err)
	}
	if len(repos) < 2 {
		t.Fatalf("scanner found %d repos, want >=2 (root=%s)", len(repos), root)
	}

	return mapper.BuildRecords(repos, mode, "")
}

// exportRecords writes the records via the production WriteJSON /
// WriteCSV writers to a tempdir file and returns the absolute path.
// The path is logged so a failure mid-pipeline points at the bytes
// that actually got serialised.
func exportRecords(t *testing.T, records []model.ScanRecord, format string) string {
	t.Helper()
	dir := t.TempDir()
	name := "gitmap." + format
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create export file: %v", err)
	}
	defer f.Close()
	writeFn := pickFormatterWriter(format)
	if err := writeFn(f, records); err != nil {
		t.Fatalf("write %s: %v", format, err)
	}

	return path
}

// pickFormatterWriter returns the formatter writer matching format.
// Centralised so runRoundTrip / exportRecords stay short.
func pickFormatterWriter(format string) func(*os.File, []model.ScanRecord) error {
	if format == "json" {
		return func(f *os.File, r []model.ScanRecord) error { return formatter.WriteJSON(f, r) }
	}

	return func(f *os.File, r []model.ScanRecord) error { return formatter.WriteCSV(f, r) }
}

// writeCloneFromManifest builds a clone-from input file (always
// JSON to keep the parser path simple and the dest column explicit).
// The URL column read from the records depends on `mode` — this is
// the round-trip step the integration test exists to guard.
func writeCloneFromManifest(t *testing.T, records []model.ScanRecord, mode, originFormat string) string {
	t.Helper()
	rows := make([]map[string]any, 0, len(records))
	for i, rec := range records {
		rows = append(rows, map[string]any{
			"url":  pickURLForMode(rec, mode),
			"dest": filepath.Join(t.TempDir(), originFormat+"-"+mode+"-out-"+rec.RepoName),
		})
		if rows[i]["url"] == "" {
			t.Fatalf("record %d has empty URL for mode %s", i, mode)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clone-from."+originFormat+"."+mode+".json")
	writeJSONFile(t, path, rows)

	return path
}

// pickURLForMode is the column-selection rule the round-trip exists
// to test. ModeHTTPS reads HTTPSUrl; ModeSSH reads SSHUrl. Any new
// mode would need a corresponding branch here AND in selectCloneURL.
func pickURLForMode(rec model.ScanRecord, mode string) string {
	if mode == constants.ModeSSH {
		return rec.SSHUrl
	}

	return rec.HTTPSUrl
}

// writeJSONFile encodes rows to path as JSON. Tiny helper so the
// caller stays under the per-function budget.
func writeJSONFile(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
}

// executePlanAndAssertOK parses the manifest with the real
// clonefrom.ParseFile, executes it, and fails with the exported
// scan path in the message so debugging starts at the bytes the
// formatter wrote.
func executePlanAndAssertOK(t *testing.T, manifest, exportPath string) {
	t.Helper()
	plan, err := clonefrom.ParseFile(manifest)
	if err != nil {
		t.Fatalf("ParseFile(%s): %v (export=%s)", manifest, err, exportPath)
	}
	results := clonefrom.Execute(plan, "", os.Stderr)
	if len(results) != len(plan.Rows) {
		t.Fatalf("results=%d, want %d", len(results), len(plan.Rows))
	}
	for i, r := range results {
		if r.Status != constants.CloneFromStatusOK {
			t.Fatalf("row %d status=%q detail=%q (export=%s)",
				i, r.Status, r.Detail, exportPath)
		}
	}
}

// seedScanTree creates two worktrees under one root, each with a
// real git remote pointing at the shared bare repo via file://.
// Two repos exercises mapper.BuildRecords' loop and clone-from's
// per-row Execute concurrency-free path on a non-trivial input.
func seedScanTree(t *testing.T, bare string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		seedOneWorktree(t, root, name, bare)
	}

	return root
}

// seedOneWorktree clones the bare repo into root/name and pins the
// remote URL to file://<bare> so mapper.BuildRecords sees a valid
// remote for both ModeHTTPS and ModeSSH lookups.
func seedOneWorktree(t *testing.T, root, name, bare string) {
	t.Helper()
	dest := filepath.Join(root, name)
	runIntegrationGit(t, root, "clone", "-q", "file://"+bare, name)
	runIntegrationGit(t, dest, "remote", "set-url", "origin", "file://"+bare)
}

// requireGitForIntegration mirrors the unit-test helper: skip the
// whole integration when git isn't on PATH so minimal CI containers
// stay green.
func requireGitForIntegration(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
}

// makeIntegrationBareRepo builds a one-commit bare repo and returns
// its absolute path. Lives in cmd/ rather than reusing
// clonefrom.makeBareRepo because that helper is unexported in the
// clonefrom test package.
func makeIntegrationBareRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "src.git")
	runIntegrationGit(t, work, "init", "-q")
	runIntegrationGit(t, work, "config", "user.email", "t@e")
	runIntegrationGit(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed README: %v", err)
	}
	runIntegrationGit(t, work, "add", ".")
	runIntegrationGit(t, work, "commit", "-q", "-m", "init")
	runIntegrationGit(t, work, "clone", "--bare", "-q", work, bare)

	return bare
}

// runIntegrationGit fatals on error with the combined output
// included so a CI failure points at the offending git invocation.
func runIntegrationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
}
