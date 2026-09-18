package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// ACR does not accept an AAD token as a docker password. It exchanges one for a
// registry refresh token, which docker then uses with this null GUID as the
// username.
const acrNullUser = "00000000-0000-0000-0000-000000000000"

// builderName is the buildx builder this creates and reuses. The default
// "docker" driver cannot export a build cache at all — it can import one and
// then fails the build rather than skipping the export — so a container driver
// is the price of caching anything between runs.
const builderName = "obtai-preview"

// BuildImage builds the image and pushes it, reusing a layer cache kept in the
// registry.
//
// The cache is what makes this worth doing on a CI runner, where the machine is
// thrown away after every job and the local layer cache is therefore always
// empty. It lives beside the images it caches, at <repository>:buildcache.
//
// This used to build with ACR Tasks, which kept Docker off the machine
// entirely. The trade was that the build became invisible: a task run streams
// to a log blob nobody is watching, so a broken Dockerfile arrived as a status
// word and the log had to be fetched afterwards to find out why. Building here
// puts the output on stdout where it is legible.
//
// Docker reads .dockerignore itself, which is why this no longer packs its own
// tarball — and why it no longer has to special-case keeping the Dockerfile in
// a context that excludes it.
func (c *Clients) BuildImage(
	ctx context.Context,
	loginServer string,
	image string,
	dockerfile string,
	contextDir string,
	buildArgs map[string]string,
) error {
	if err := c.dockerLogin(ctx, loginServer); err != nil {
		return err
	}
	if err := ensureBuilder(ctx); err != nil {
		return err
	}

	cache := cacheRef(image)

	args := []string{
		"buildx", "build",
		"--builder", builderName,
		// The container app runs linux/amd64. Without this an arm64 laptop
		// builds for itself and pushes an image the app cannot start, with
		// nothing failing until the revision refuses to come up.
		"--platform", "linux/amd64",
		"--file", dockerfile,
		"--tag", image,
		// Straight to the registry. A container builder keeps nothing in the
		// local image store, so exporting there first would be a write nobody
		// reads — it was the single slowest step of a build.
		"--push",
		"--cache-from", "type=registry,ref=" + cache,
		// mode=max caches intermediate stages too. The expensive one here is
		// `npm ci`, which lives in a stage the final image only copies from, so
		// the default mode=min would cache none of it.
		//
		// image-manifest + oci-mediatypes write the cache as an OCI image
		// manifest rather than an image index, which is the form registries
		// that reject the index format accept.
		//
		// ignore-error because a registry that refuses the cache must degrade
		// to a slow build, not a failed one.
		"--cache-to", "type=registry,ref=" + cache +
			",mode=max,image-manifest=true,oci-mediatypes=true,ignore-error=true",
	}

	// Sorted, so a rebuild with the same inputs produces the same command.
	names := make([]string, 0, len(buildArgs))
	for name := range buildArgs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "--build-arg", name+"="+buildArgs[name])
	}

	args = append(args, contextDir)

	if err := c.docker(ctx, args...); err != nil {
		return fmt.Errorf("building %s: %w", image, err)
	}
	return nil
}

// cacheRef is the image reference with its tag replaced by "buildcache", so the
// cache sits in the same repository as the images.
//
// Teardown deletes tags matching the pull request's own prefix, so this one is
// never swept up with them. Two previews building at once both write it and the
// last one wins, which costs a cache generation rather than correctness.
func cacheRef(image string) string {
	if at := strings.LastIndex(image, ":"); at > strings.LastIndex(image, "/") {
		return image[:at] + ":buildcache"
	}
	return image + ":buildcache"
}

// ensureBuilder creates the container-driver builder once and reuses it after.
// Creating it costs a few seconds on a fresh machine and nothing on a warm one.
func ensureBuilder(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", builderName).Run() == nil {
		return nil
	}
	create := exec.CommandContext(ctx, "docker", "buildx", "create",
		"--name", builderName, "--driver", "docker-container")
	if output, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("creating the %s buildx builder: %w\n%s",
			builderName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// docker runs the CLI with its output left attached to the terminal. The build
// log is the point of building locally, so it is not captured.
func (c *Clients) docker(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = c.Log
	command.Stderr = os.Stderr
	return command.Run()
}

// dockerLogin signs the local daemon in to the registry using the extension's
// own credential, rather than shelling out to `az acr login` — so there is one
// answer to "who is this running as", and no dependency on the az CLI.
func (c *Clients) dockerLogin(ctx context.Context, loginServer string) error {
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return fmt.Errorf("getting a token for %s: %w", loginServer, err)
	}

	form := url.Values{
		"grant_type":   {"access_token"},
		"service":      {loginServer},
		"access_token": {token.Token},
	}
	if tenant := os.Getenv("AZURE_TENANT_ID"); tenant != "" {
		form.Set("tenant", tenant)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+loginServer+"/oauth2/exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("exchanging a token with %s: %w", loginServer, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("exchanging a token with %s: %s: %s",
			loginServer, response.Status, strings.TrimSpace(string(detail)))
	}

	var exchanged struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&exchanged); err != nil {
		return fmt.Errorf("reading the token %s returned: %w", loginServer, err)
	}
	if exchanged.RefreshToken == "" {
		return fmt.Errorf("%s returned no refresh token", loginServer)
	}

	// --password-stdin: a password in argv is visible to every process on the
	// machine, and docker warns about it.
	login := exec.CommandContext(ctx, "docker", "login",
		"--username", acrNullUser, "--password-stdin", loginServer)
	login.Stdin = strings.NewReader(exchanged.RefreshToken)

	if output, err := login.CombinedOutput(); err != nil {
		return fmt.Errorf("signing docker in to %s: %w\n%s",
			loginServer, err, strings.TrimSpace(string(output)))
	}
	return nil
}
