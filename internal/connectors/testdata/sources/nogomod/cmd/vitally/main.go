// Command vitally is the test program of a link connector.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("vitally 0.1.0")
		return
	}
	fmt.Println("usage: vitally <command>")
}
