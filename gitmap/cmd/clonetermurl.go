package cmd

// clonetermurl.go — convenience adapter for commands that have just
// a (URL, dest) pair on hand and need to emit the standardized
// RepoTermBlock. Centralizes the ls-remote branch detection so
// every clone command produces the same block shape.

import (
	"github.com/alimtvnetwork/gitmap-v7/gitmap/clonenext"
)

// printCloneTermBlockForURL detects the remote default branch (best
// effort, with timeout) and prints the standardized block. dest may
// be empty — when so we derive a sensible repo name from the URL
// and the renderer falls back to "(unknown)" for missing fields.
//
// idx is the 1-based row number printed in the block header. For
// single-URL commands callers pass 1; for multi-URL/batch commands
// callers pass the loop index + 1.
//
// No-op when output != "terminal" (the underlying helper short-
// circuits) so it's safe to call unconditionally on the hot path.
func printCloneTermBlockForURL(output string, idx int, url, dest string) {
	name := repoNameFromURL(url)
	if len(name) == 0 {
		name = url
	} else {
		// Mirror executeDirectClone's flatten logic so the block
		// surfaces the SAME folder name the user will see on disk.
		parsed := clonenext.ParseRepoName(name)
		if parsed.HasVersion {
			name = parsed.BaseName
		}
	}
	branch := detectRemoteHEAD(url)
	maybePrintCloneTermBlock(output, CloneTermBlockInput{
		Index:        idx,
		Name:         name,
		Branch:       branch,
		BranchSource: remoteBranchSource(branch),
		OriginalURL:  url,
		TargetURL:    url,
		Dest:         dest,
	})
}
