package azure

import (
	"context"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
)

// TagsWithPrefix lists a repository's tags that start with prefix.
func (c *Clients) TagsWithPrefix(ctx context.Context, loginServer, repository, prefix string) ([]string, error) {
	client, err := azcontainerregistry.NewClient("https://"+loginServer, c.credential, nil)
	if err != nil {
		return nil, err
	}

	var found []string
	pager := client.NewListTagsPager(repository, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			// A repository that has never been pushed to is not an error here:
			// a pull request whose build never succeeded has no tags to clean.
			if NotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, tag := range page.Tags {
			if tag.Name != nil && strings.HasPrefix(*tag.Name, prefix) {
				found = append(found, *tag.Name)
			}
		}
	}
	return found, nil
}

// DeleteTag removes a tag.
//
// Untag only: the manifest is left to the registry's retention policy rather
// than force-deleted, so a layer shared with another tag cannot be pulled out
// from under it.
func (c *Clients) DeleteTag(ctx context.Context, loginServer, repository, tag string) error {
	client, err := azcontainerregistry.NewClient("https://"+loginServer, c.credential, nil)
	if err != nil {
		return err
	}
	_, err = client.DeleteTag(ctx, repository, tag, nil)
	if err != nil && NotFound(err) {
		return nil
	}
	return err
}
