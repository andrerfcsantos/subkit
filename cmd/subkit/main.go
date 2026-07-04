package main

import (
	"fmt"
	"os"

	"github.com/andrerfcsantos/subkit-codex/internal/app"
)

func main() {
	if err := app.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
