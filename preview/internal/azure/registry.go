package azure

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerregistry/armcontainerregistry"
)

// BuildImage builds an image with ACR Tasks and pushes it to the registry: ask
// for a source upload URL, tar the build context, upload it, schedule a build.
// The build happens in Azure, so no Docker daemon is involved anywhere.
func (c *Clients) BuildImage(
	ctx context.Context,
	registryName string,
	image string,
	dockerfile string,
	contextDir string,
	buildArgs map[string]string,
) error {
	upload, err := c.Registries.GetBuildSourceUploadURL(ctx, c.ResourceGroup, registryName, nil)
	if err != nil {
		return fmt.Errorf("asking %s for a source upload url: %w", registryName, err)
	}
	if upload.UploadURL == nil || upload.RelativePath == nil {
		return fmt.Errorf("%s returned no source upload location", registryName)
	}

	tarball, err := os.CreateTemp("", "azd-preview-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tarball.Name())
	defer tarball.Close()

	if err := writeContext(tarball, contextDir, dockerfile); err != nil {
		return fmt.Errorf("packing the build context: %w", err)
	}
	if _, err := tarball.Seek(0, io.SeekStart); err != nil {
		return err
	}

	packed, err := tarball.Stat()
	if err != nil {
		return err
	}
	if err := putBlob(ctx, *upload.UploadURL, tarball, packed.Size()); err != nil {
		return fmt.Errorf("uploading the build context: %w", err)
	}

	arguments := make([]*armcontainerregistry.Argument, 0, len(buildArgs))
	for name, value := range buildArgs {
		arguments = append(arguments, &armcontainerregistry.Argument{
			Name:     to.Ptr(name),
			Value:    to.Ptr(value),
			IsSecret: to.Ptr(false),
		})
	}

	request := &armcontainerregistry.DockerBuildRequest{
		Type:           to.Ptr("DockerBuildRequest"),
		SourceLocation: upload.RelativePath,
		DockerFilePath: to.Ptr(dockerfile),
		ImageNames:     []*string{to.Ptr(image)},
		IsPushEnabled:  to.Ptr(true),
		Platform: &armcontainerregistry.PlatformProperties{
			OS:           to.Ptr(armcontainerregistry.OSLinux),
			Architecture: to.Ptr(armcontainerregistry.ArchitectureAmd64),
		},
		Arguments: arguments,
	}

	poller, err := c.Registries.BeginScheduleRun(ctx, c.ResourceGroup, registryName, request, nil)
	if err != nil {
		return fmt.Errorf("scheduling the build: %w", err)
	}

	scheduled, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("scheduling the build: %w", err)
	}
	if scheduled.Properties == nil || scheduled.Properties.RunID == nil {
		return fmt.Errorf("%s scheduled a build but returned no run id", registryName)
	}

	return c.waitForRun(ctx, registryName, *scheduled.Properties.RunID)
}

// waitForRun blocks until an ACR task run reaches a terminal state.
// BeginScheduleRun's poller completes when the run has been scheduled, not when
// it has finished, so the run has to be polled separately.
func (c *Clients) waitForRun(ctx context.Context, registryName, runID string) error {
	const (
		timeout  = 30 * time.Minute
		interval = 5 * time.Second
	)

	status := armcontainerregistry.RunStatusQueued
	reported := ""

	finished := Until(ctx, timeout, interval, func(ctx context.Context) bool {
		run, err := c.Runs.Get(ctx, c.ResourceGroup, registryName, runID, nil)
		if err != nil {
			// The deadline is the backstop for a transient read failure.
			return false
		}
		if run.Properties == nil || run.Properties.Status == nil {
			return false
		}
		status = *run.Properties.Status

		if string(status) != reported {
			reported = string(status)
			fmt.Printf("    %s…\n", reported)
		}

		switch status {
		case armcontainerregistry.RunStatusSucceeded,
			armcontainerregistry.RunStatusFailed,
			armcontainerregistry.RunStatusCanceled,
			armcontainerregistry.RunStatusError,
			armcontainerregistry.RunStatusTimeout:
			return true
		default:
			return false
		}
	})

	if !finished {
		return fmt.Errorf("build run %s in %s did not finish within %s (last status %s)",
			runID, registryName, timeout, status)
	}
	if status == armcontainerregistry.RunStatusSucceeded {
		return nil
	}

	// Without the log the only evidence is a status word.
	if logs, err := c.runLog(ctx, registryName, runID); err == nil && logs != "" {
		fmt.Println(logs)
	}
	return fmt.Errorf("image build %s — run %s in %s", status, runID, registryName)
}

// runLog fetches the tail of a run's log.
func (c *Clients) runLog(ctx context.Context, registryName, runID string) (string, error) {
	result, err := c.Runs.GetLogSasURL(ctx, c.ResourceGroup, registryName, runID, nil)
	if err != nil || result.LogLink == nil {
		return "", err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, *result.LogLink, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n"), nil
}

// writeContext tars and gzips a build context, honouring .dockerignore. The
// Dockerfile is always included whatever .dockerignore says: Docker
// special-cases the file named by -f, but ACR Tasks reads it out of the
// uploaded context.
func writeContext(out io.Writer, root, dockerfile string) error {
	ignore, err := loadDockerignore(root)
	if err != nil {
		return err
	}

	zip := gzip.NewWriter(out)
	defer zip.Close()
	archive := tar.NewWriter(zip)
	defer archive.Close()

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.ToSlash(relative)

		if relative != filepath.ToSlash(dockerfile) && ignore.matches(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Following a symlink is how a context accidentally includes a home
		// directory.
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = relative

		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(archive, file)
		return err
	})
}

func putBlob(ctx context.Context, url string, body *os.File, size int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	// The SAS URL carries its own authorisation.
	request.Header.Set("x-ms-blob-type", "BlockBlob")

	// Set explicitly: Go would otherwise send an *os.File chunked, which blob
	// storage rejects as an unsupported header.
	request.ContentLength = size

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}
