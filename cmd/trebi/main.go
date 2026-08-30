package main

import (
	"context"
	"fmt"
	"os"

	"github.com/trebi-ai/trebi/internal/cli"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
