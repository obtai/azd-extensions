// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

// The go:generate directive lives at the module root because schemagen resolves
// its source tree and output relative to the annotated file's directory.
//go:generate go run ./cmd/schemagen

package main

import (
	"github.com/obtai/azd-extensions/preview/internal/cmd"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

func main() {
	azdext.Run(cmd.NewRootCommand())
}
