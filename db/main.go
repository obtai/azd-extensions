package main

import (
	"github.com/obtai/azd-extensions/db/internal/cmd"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

func main() {
	azdext.Run(cmd.NewRootCommand())
}
