package artifactversions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteVersionsRemovesOnePathAndKeepsSharedBlobs(t *testing.T) {
	h := newHarness(t)
	h.write("report.html", "<h1>v1</h1>")
	h.write("copy.html", "<h1>v1</h1>")
	h.tick()
	h.tick()
	h.write("report.html", "<h1>v2</h1>")
	h.tick()
	h.tick()
	if len(h.versions("report.html")) != 2 || h.blobCount() != 2 {
		t.Fatalf("setup: versions=%d blobs=%d, want 2 and 2", len(h.versions("report.html")), h.blobCount())
	}
	shared := h.versions("copy.html")[0].SHA256
	own := h.versions("report.html")[0].SHA256

	if err := os.Remove(filepath.Join(h.dir, "report.html")); err != nil {
		t.Fatal(err)
	}
	if err := h.cap.DeleteVersions(h.ctx, h.sess, "report.html"); err != nil {
		t.Fatal(err)
	}

	if vs := h.versions("report.html"); len(vs) != 0 {
		t.Fatalf("versions after delete = %+v, want none", vs)
	}
	if _, ok := h.blob(own); ok {
		t.Fatal("blob only report.html used is still stored")
	}
	if _, ok := h.blob(shared); !ok {
		t.Fatal("blob copy.html still uses was removed")
	}
	if vs := h.versions("copy.html"); len(vs) != 1 {
		t.Fatalf("copy.html versions = %d, want 1", len(vs))
	}
	h.tick()
	h.tick()
	if vs := h.versions("report.html"); len(vs) != 0 {
		t.Fatalf("versions after a later capture = %+v, want none", vs)
	}
}
