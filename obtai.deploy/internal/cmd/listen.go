// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"

	"obtai.deploy/internal/azure"
	"obtai.deploy/internal/config"
)

// newListenCommand registers the azd lifecycle handlers.
//
// These replace the shell hooks a repository would otherwise wire into
// azure.yaml. The order is the extension's, not the repository's: a release
// that seeds secrets after the infrastructure references them, or declares
// success before a revision is healthy, is a different and worse thing.
func newListenCommand() *cobra.Command {
	return azdext.NewListenCommand(func(host *azdext.ExtensionHost) {
		host.
			WithProjectEventHandler("preprovision", onPreprovision).
			WithProjectEventHandler("postprovision", onPostprovision).
			WithProjectEventHandler("predeploy", onPredeploy).
			WithProjectEventHandler("postdeploy", onPostdeploy)
	})
}

// onPreprovision seeds the secrets the infrastructure references, once.
//
// An app references its Key Vault secrets by name, and a reference to a name
// the vault does not hold FAILS THE REVISION outright — it does not degrade.
// But the vault is created by the same deployment, so a brand-new environment
// has nothing to reference yet. Handing the values to the template as secure
// parameters, which ARM redacts from deployment history, lets vault, secrets
// and app deploy together.
//
// Only ever seeds what is MISSING, and postprovision clears the values
// afterwards, so the next provision passes empty strings and the template
// leaves the vault alone. That is what stops a routine provision rotating a
// signing key and logging every user out.
func onPreprovision(ctx context.Context, args *azdext.ProjectEventArgs) error {
	settings, err := config.Load(".")
	if err != nil || len(settings.Secrets) == 0 {
		// A repository with no required secrets has nothing to seed, and one
		// with no preview.yaml is not using this extension for provisioning.
		return nil
	}

	client, err := azdext.NewAzdClient()
	if err != nil {
		return err
	}
	defer client.Close()

	current, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return err
	}
	name := current.Environment.Name

	values, err := outputs(ctx, client, name)
	if err != nil {
		return err
	}

	prefix := environmentPrefix(args.Project.Name)
	vault := values[prefix+"_KEY_VAULT_NAME"]

	fmt.Printf("==> preprovision: %s\n", name)
	if vault == "" {
		// A first provision has no outputs yet, so there is nothing to check
		// against and everything is seeded.
		fmt.Println("    no vault yet — first provision")
	} else {
		fmt.Printf("    vault %s — only missing secrets will be seeded\n", vault)
	}

	clients, err := azure.New(ctx, values["AZURE_SUBSCRIPTION_ID"], values["AZURE_RESOURCE_GROUP"])
	if err != nil {
		return err
	}

	var seeded []string
	for _, envVar := range settings.Secrets {
		secretName := config.VaultName(envVar)
		present := false

		if vault != "" {
			secrets, err := clients.Secrets(vault)
			if err != nil {
				return err
			}
			_, err = secrets.GetSecret(ctx, secretName, "", nil)
			switch {
			case err == nil:
				present = true
			case azure.NotFound(err):
				present = false
			case azure.Forbidden(err):
				// "Not found" and "not allowed to look" are different answers,
				// and conflating them is dangerous: an operator without
				// data-plane access to a POPULATED vault would be told
				// everything is missing, and would then overwrite live secrets.
				return fmt.Errorf(
					"no permission to read secrets in %s, so this cannot tell whether %q "+
						"already exists.\nIf the vault is genuinely empty — a new environment, "+
						"or a provision that failed before seeding — grant yourself Key Vault "+
						"Secrets Officer and re-run.\nNever seed blind against a live "+
						"environment: it overwrites every required secret", vault, secretName)
			default:
				return err
			}
		}

		value := ""
		if present {
			fmt.Printf("    %s: already set, leaving alone\n", secretName)
			// Explicitly empty, not merely absent: an empty secure parameter is
			// what tells the template to skip its seeding module entirely.
		} else {
			value, err = generate()
			if err != nil {
				return err
			}
			seeded = append(seeded, secretName)
		}

		if _, err := client.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: name,
			Key:     envVar,
			Value:   value,
		}); err != nil {
			return err
		}
	}

	if len(seeded) > 0 {
		fmt.Printf("    seeding: %v\n", seeded)
	}
	return nil
}

// onPostprovision takes the generated values back out of the azd environment.
//
// They live in a plaintext file and are only needed for the length of one
// provision. Clearing them is load-bearing rather than tidy: it is what makes
// the next provision a no-op on the vault.
func onPostprovision(ctx context.Context, args *azdext.ProjectEventArgs) error {
	settings, err := config.Load(".")
	if err != nil || len(settings.Secrets) == 0 {
		return nil
	}

	client, err := azdext.NewAzdClient()
	if err != nil {
		return err
	}
	defer client.Close()

	current, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return err
	}

	for _, envVar := range settings.Secrets {
		if _, err := client.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: current.Environment.Name,
			Key:     envVar,
			Value:   "",
		}); err != nil {
			return err
		}
	}

	fmt.Println("==> postprovision: cleared seeded secret values from the environment")
	return nil
}

// onPredeploy migrates, before anything new can serve.
//
// Runs after the image is pushed and before the app is rolled, so a failure
// fails the release with the previous revision still serving. There is no
// in-cluster job: this runs wherever the deploy runs, and therefore
// authenticates as the CALLER rather than as the app.
func onPredeploy(ctx context.Context, args *azdext.ProjectEventArgs) error {
	settings, err := config.Load(".")
	if err != nil || settings.Provision == "" {
		return nil
	}

	client, err := azdext.NewAzdClient()
	if err != nil {
		return err
	}
	defer client.Close()

	current, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return err
	}
	name := current.Environment.Name

	target, err := targetFor(ctx, client, args.Project.Name, name)
	if err != nil {
		return err
	}

	prefix := environmentPrefix(args.Project.Name)
	fmt.Printf("==> predeploy: %s\n", name)

	return target.Provision(ctx,
		target.Outputs[prefix+"_APP_ENV"],
		target.Outputs[prefix+"_APP_URL"],
		target.Outputs[prefix+"_POSTGRES_DATABASE"],
	)
}

// onPostdeploy proves the revision azd just created is actually healthy.
//
// Container Apps keeps the previous revision serving when a new one fails to
// come up, so this is not what protects the environment — it is what stops a
// broken deploy reporting a green tick.
func onPostdeploy(ctx context.Context, args *azdext.ProjectEventArgs) error {
	client, err := azdext.NewAzdClient()
	if err != nil {
		return err
	}
	defer client.Close()

	current, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return err
	}
	name := current.Environment.Name

	values, err := outputs(ctx, client, name)
	if err != nil {
		return err
	}

	prefix := environmentPrefix(args.Project.Name)
	app := values[prefix+"_CONTAINER_APP_NAME"]
	if app == "" {
		return nil
	}

	clients, err := azure.New(ctx, values["AZURE_SUBSCRIPTION_ID"], values["AZURE_RESOURCE_GROUP"])
	if err != nil {
		return err
	}

	return azure.WaitHealthy(ctx, clients, app)
}

// generate returns 48 hex characters — 192 bits. Comfortably over the
// 32-character minimum a session-signing key usually wants, and over any
// password policy.
func generate() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
