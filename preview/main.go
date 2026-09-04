// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

// The schema for preview.yaml is generated from internal/config.Config. The
// directive lives here, at the module root, because `go generate` runs a
// command with the working directory of the file it annotates — and schemagen
// resolves both its source tree and its output relative to that.
//go:generate go run ./cmd/schemagen

package main

import (
	"github.com/obtai/azd-extensions/preview/internal/cmd"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

func main() {
	azdext.Run(cmd.NewRootCommand())
}
