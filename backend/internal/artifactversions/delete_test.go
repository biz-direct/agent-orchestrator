package artifactversions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func (h *harness) dataDir() string {
	return filepath.Dir(filepath.Dir(h.dir))
}

func TestDeleteSessionRemovesFilesAndVersionsAndKeepsAnEmptyDir(t *testing.T) {
	h := newHarness(t)
	h.write("report.html", "<h1>v1</h1>")
	h.tick()
	h.tick()
	if len(h.versions("report.html")) != 1 || h.blobCount() != 1 {
		t.Fatalf("setup: versions=%d blobs=%d, want one captured version", len(h.versions("report.html")), h.blobCount())
	}

	if err := h.cap.DeleteSession(h.ctx, h.sess.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatalf("artifact dir should remain for a restore: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("artifact dir entries = %v, want empty", entries)
	}
	if vs := h.versions("report.html"); len(vs) != 0 {
		t.Fatalf("versions after delete = %+v, want none", vs)
	}
	if trash, _ := os.ReadDir(filepath.Join(h.dataDir(), trashDir)); len(trash) != 0 {
		t.Fatalf("trash entries = %v, want the moved dir removed", trash)
	}
	// A capture queued behind the delete finds nothing to record.
	h.tick()
	if vs := h.versions("report.html"); len(vs) != 0 {
		t.Fatalf("versions after a later capture = %+v, want none", vs)
	}
}

func TestDeleteSessionLeavesEverythingWhenTheMoveFails(t *testing.T) {
	h := newHarness(t)
	h.write("report.html", "<h1>v1</h1>")
	h.tick()
	h.tick()
	// A file where the trash directory belongs makes the move fail up front.
	if err := os.WriteFile(filepath.Join(h.dataDir(), trashDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.cap.DeleteSession(h.ctx, h.sess.ID); err == nil {
		t.Fatal("DeleteSession succeeded, want an error")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "report.html")); err != nil {
		t.Fatalf("artifact removed by a failed delete: %v", err)
	}
	if vs := h.versions("report.html"); len(vs) != 1 {
		t.Fatalf("versions after a failed delete = %d, want 1", len(vs))
	}
}

func TestDeleteSessionEmptiesLeftoverTrash(t *testing.T) {
	h := newHarness(t)
	leftover := filepath.Join(h.dataDir(), trashDir, "old-1", "page.html")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.cap.DeleteSession(h.ctx, h.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(leftover)); !os.IsNotExist(err) {
		t.Fatalf("leftover trash still present: %v", err)
	}
}

type failingDeleteStore struct{ versionStore }

func (failingDeleteStore) DeleteSessionArtifactVersions(context.Context, domain.SessionID) error {
	return errors.New("db is locked")
}

func TestDeleteSessionRestoresFilesWhenDroppingVersionRowsFails(t *testing.T) {
	h := newHarness(t)
	h.write("report.html", "<h1>v1</h1>")
	h.tick()
	h.tick()
	h.cap.store = failingDeleteStore{h.cap.store}

	if err := h.cap.DeleteSession(h.ctx, h.sess.ID); err == nil {
		t.Fatal("DeleteSession succeeded, want the store error")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "report.html")); err != nil {
		t.Fatalf("artifact not restored after a late failure: %v", err)
	}
	if vs := h.versions("report.html"); len(vs) != 1 {
		t.Fatalf("versions after a failed delete = %d, want 1", len(vs))
	}
	if trash, _ := os.ReadDir(filepath.Join(h.dataDir(), trashDir)); len(trash) != 0 {
		t.Fatalf("trash entries = %v, want the files moved back", trash)
	}
}

// blockingDeleteStore fails the row delete after leaving a file in the
// recreated directory, which makes restoring the moved copy fail too.
type blockingDeleteStore struct {
	versionStore
	dir string
}

func (s blockingDeleteStore) DeleteSessionArtifactVersions(context.Context, domain.SessionID) error {
	_ = os.WriteFile(filepath.Join(s.dir, "late.txt"), []byte("x"), 0o600)
	return errors.New("db is locked")
}

func TestDeleteSessionKeepsAnUnrestorableCopyOutOfTheTrashSweep(t *testing.T) {
	h := newHarness(t)
	h.write("report.html", "<h1>v1</h1>")
	h.tick()
	h.tick()
	h.cap.store = blockingDeleteStore{versionStore: h.cap.store, dir: h.dir}

	if err := h.cap.DeleteSession(h.ctx, h.sess.ID); err == nil {
		t.Fatal("DeleteSession succeeded, want the store error")
	}
	// Another delete sweeps the trash; the quarantined copy must survive it.
	h.cap.store = failingDeleteStore{h.cap.store.(blockingDeleteStore).versionStore}
	_ = h.cap.DeleteSession(h.ctx, "other-session")
	found := false
	_ = filepath.WalkDir(filepath.Join(h.dataDir(), trashDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "report.html" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("the only copy of the artifacts was swept from the trash")
	}
}
