package main

import (
	"fmt"
	"os"

	"github.com/trebi-ai/trebi/internal/eval"
)

func main() {
	if err := eval.NewApp().Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
