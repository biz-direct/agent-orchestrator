package artifactversions

import (
	"cmp"
	"context"
	"fmt"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionartifacts"
)

// DeleteVersions deletes the version rows of one artifact path, and each of
// their blobs that no other version of the session still refers to, after the
// user deleted that file.
//
// It holds the capture lock, so a capture that read the file before it was
// removed records its version first and that version is deleted here; a later
// capture finds no file.
func (c *Capturer) DeleteVersions(ctx context.Context, sess domain.SessionRecord, relPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.observed[sess.ID], relPath)
	sums, err := c.store.DeleteSessionArtifactPathVersions(ctx, sess.ID, relPath)
	if err != nil || len(sums) == 0 {
		return err
	}
	root, err := os.OpenRoot(cmp.Or(sess.Metadata.ArtifactDir, sessionartifacts.Dir(c.dataDir, sess.ID)))
	if err != nil {
		return fmt.Errorf("delete artifact versions %s %q: %w", sess.ID, relPath, err)
	}
	defer func() { _ = root.Close() }()
	blobs := blobStore{root: root}
	removed := make(map[string]bool, len(sums))
	for _, sum := range sums {
		if !removed[sum] {
			removed[sum] = true
			c.removeUnreferenced(ctx, sess.ID, blobs, sum)
		}
	}
	return nil
}
