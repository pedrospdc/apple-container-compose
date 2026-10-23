// Command container-compose is Docker Compose for Apple container.
//
// It can be used standalone (`container-compose up`) or installed as an
// Apple container CLI plugin (`container compose up`).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pedrospdc/apple-container-compose/internal/cli"
)

func main() {
	name := filepath.Base(os.Args[0])
	if name == "compose" {
		name = "container compose"
	}
	root := cli.NewRootCommand(name)
	if err := root.ExecuteContext(context.Background()); err != nil {
		var ec cli.ExitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
