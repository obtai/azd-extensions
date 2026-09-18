package main

import (
	"github.com/obtai/azd-extensions/shell/internal/cmd"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

func main() {
	azdext.Run(cmd.NewRootCommand())
}
