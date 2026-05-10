package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// generateManPages writes one .1 file per cobra command into outDir. Used
// by the goreleaser before-hook so released archives ship man pages.
func generateManPages(root *cobra.Command, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	header := &doc.GenManHeader{
		Title:   "TUNNEL",
		Section: "1",
		Source:  "tunnel " + version,
	}
	return doc.GenManTree(root, header, outDir)
}
