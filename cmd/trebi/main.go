package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/trebi-ai/trebi/internal/cli"
)

func init() { //nolint:gochecknoinits // NSStatusBar must run on the process main OS thread
	// Pin before main: Go 1.14+ async preemption can migrate the main
	// goroutine off thread 0 during imported-package init. menuet then
	// SIGABRTs in NSStatusBarWindow ("NSWindow should only be instantiated
	// on the main thread").
	runtime.LockOSThread()
}

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
