// Package app is the only public entry point. cmd/trebi and a wrapping
// edition both call Main; nothing else is exported.
package app

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/cli"
)

func init() { //nolint:gochecknoinits // NSStatusBar must run on the process main OS thread
	// Pin before main: Go 1.14+ async preemption can migrate the main
	// goroutine off thread 0 during imported-package init. menuet then
	// SIGABRTs in NSStatusBarWindow ("NSWindow should only be instantiated
	// on the main thread").
	runtime.LockOSThread()
}

// Main runs the CLI with os.Args and exits the process. Both mains are
// this one line, so error printing and exit codes cannot drift.
func Main(set ext.Set) {
	if err := cli.NewApp(cli.Options{Ext: set}).Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
