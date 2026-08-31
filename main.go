package main

import (
	"os"

	"mtdiff/cmd/mtdiff"
)

func main() {
	os.Exit(mtdiff.Execute())
}
