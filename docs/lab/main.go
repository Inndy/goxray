// Command lab measures blob-storage layouts against a real goxray store.
// It is the harness behind docs/blob-storage.md; nothing here is shipped.
//
//	GOXRAY_DIR=~/.goxray go run . design 4k
//
// Subcommands: stats content shape scale holdout blast micro recipe
// bench <req|res|both> [tag] baseline design [2k|4k|8k|nodict|dict1m|nofsync]
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// workDir is where design/baseline materialise a real store to verify against.
func workDir() string {
	if d := os.Getenv("GOXRAY_LAB_WORK"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "goxray-lab")
}

func main() {
	root := os.Getenv("GOXRAY_DIR")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".goxray")
	}
	if _, err := os.Stat(filepath.Join(root, "manifests")); err != nil {
		fmt.Fprintf(os.Stderr, "no store at %s (set GOXRAY_DIR)\n", root)
		os.Exit(1)
	}
	cmd := "stats"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	args := os.Args[2:]
	switch cmd {
	case "stats":
		cmdStats(root)
	case "content":
		cmdContent(root)
	case "shape":
		cmdShape(root)
	case "scale":
		cmdScale(root)
	case "holdout":
		cmdHoldout(root)
	case "blast":
		cmdBlast(root)
	case "micro":
		cmdMicro(root)
	case "recipe":
		cmdRecipe(root)
	case "baseline":
		cmdBaseline(root, args)
	case "generations":
		cmdGenerations(root, args)
	case "archive":
		cmdArchive(root, args)
	case "design":
		cmdDesign(root, args)
	case "bench":
		cmdBench(root, args)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand", cmd)
		os.Exit(1)
	}
}
