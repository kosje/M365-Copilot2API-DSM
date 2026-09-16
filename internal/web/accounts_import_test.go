package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// zeroStream yields zeros forever, so a test can build a highly compressible
// payload without materialising it in memory.
type zeroStream struct{}

func (zeroStream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func buildArchive(t *testing.T, name string, size int64, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	} else if _, err := io.CopyN(tw, zeroStream{}, size); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// maxImportBytes bounds the upload, but gzip expands by orders of magnitude: a
// 16 MiB archive can decompress to gigabytes. Without a limit on the
// decompressed member, importing such a file allocates until the process dies -
// and on a NAS that gets the whole gateway killed, accounts and all.
func TestAccountsFromArchiveRejectsDecompressionBomb(t *testing.T) {
	const declared = 64 << 20 // 64 MiB of zeros, a few hundred KiB compressed
	archive := buildArchive(t, "m365-migration/data/accounts.json", declared, nil)
	if int64(len(archive)) >= declared {
		t.Fatalf("archive did not compress enough to be a useful test: %d bytes", len(archive))
	}

	_, err := accountsFromArchive(archive)
	if err == nil {
		t.Fatal("expected a decompression bomb to be rejected")
	}
	if !strings.Contains(err.Error(), "过大") && !strings.Contains(err.Error(), "上限") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The guard must not break the feature it protects.
func TestAccountsFromArchiveAcceptsNormalArchive(t *testing.T) {
	payload := []byte(`{"accounts":[{"id":"a@example.com","email":"a@example.com"}]}`)
	archive := buildArchive(t, "m365-migration/data/accounts.json", int64(len(payload)), payload)

	got, err := accountsFromArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

// Members that are not accounts.json are skipped, and the reader must still
// find the real one behind them.
func TestAccountsFromArchiveSkipsOtherMembers(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	filler := []byte("noise")
	if err := tw.WriteHeader(&tar.Header{
		Name: "m365-migration/README.md", Mode: 0o600, Size: int64(len(filler)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(filler); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"accounts":[{"id":"b@example.com"}]}`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "m365-migration/data/accounts.json", Mode: 0o600, Size: int64(len(payload)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := accountsFromArchive(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}
