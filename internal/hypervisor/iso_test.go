package hypervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeISOServer stands up a tiny HTTP server that serves a mock ISO +
// matching SHA256SUMS so we can exercise the full Fetch flow without
// hitting Ubuntu's CDN. Returns base URL, sha hex, raw ISO bytes.
func fakeISOServer(t *testing.T) (*httptest.Server, string, []byte) {
	t.Helper()
	body := []byte("PRETEND-ISO-CONTENT-" + strings.Repeat("X", 8192))
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	// Match the real Ubuntu layout: /<release>/<file>
	mux.HandleFunc("/22.04/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s *ubuntu-22.04.5-live-server-amd64.iso\n", digest)
	})
	mux.HandleFunc("/22.04/ubuntu-22.04.5-live-server-amd64.iso", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, digest, body
}

// patchedFetcher is a small helper: builds an ISOFetcher pointed at
// the test HTTP server with a CacheDir under TempDir, suppresses the
// userHomeDir lookup, and returns it ready to Fetch.
func patchedFetcher(t *testing.T, srv *httptest.Server) *ISOFetcher {
	t.Helper()
	dir := t.TempDir()
	// Substitute the URL builder so isoURL/fetchSHA256SUMS hit the
	// fake server. We do this by overriding the fetcher's HTTPClient
	// to a transport that rewrites the host portion.
	transport := &rewriteTransport{prefix: srv.URL}
	return &ISOFetcher{
		Version:    "22.04.5",
		Arch:       "amd64",
		CacheDir:   dir,
		HTTPClient: &http.Client{Transport: transport},
	}
}

// rewriteTransport intercepts requests to releases.ubuntu.com / cdimage
// and redirects them at the test server. The path is preserved.
type rewriteTransport struct {
	prefix string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Build a fresh request hitting the test server with the original path.
	newURL := rt.prefix + req.URL.Path
	r, err := http.NewRequest(req.Method, newURL, req.Body)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(r)
}

func TestDetectArch_KnownArchs(t *testing.T) {
	got, err := detectArch()
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if err != nil {
			t.Fatalf("expected no error on %s, got %v", runtime.GOARCH, err)
		}
		if got != runtime.GOARCH {
			t.Errorf("got %q, want %q", got, runtime.GOARCH)
		}
		return
	}
	if err == nil {
		t.Errorf("expected error on unsupported arch %s", runtime.GOARCH)
	}
}

func TestBaseRelease(t *testing.T) {
	cases := map[string]string{
		"22.04.5":   "22.04",
		"22.04":     "22.04",
		"22.04.5.1": "22.04",
		"24.04":     "24.04",
		"weird":     "weird",
	}
	for in, want := range cases {
		if got := baseRelease(in); got != want {
			t.Errorf("baseRelease(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	body := []byte(`# comment
deadbeef1234 *some-other-file.iso
abc123def *ubuntu-22.04.5-live-server-amd64.iso
malformed-line
99 too-few-fields
`)
	got, ok := parseSHA256SUMS(body, "ubuntu-22.04.5-live-server-amd64.iso")
	if !ok || got != "abc123def" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	if _, ok := parseSHA256SUMS(body, "missing.iso"); ok {
		t.Error("missing file should return ok=false")
	}
}

func TestParseSHA256SUMS_NoStarPrefix(t *testing.T) {
	body := []byte("aaaa  ubuntu-22.04.5-live-server-amd64.iso\n")
	got, ok := parseSHA256SUMS(body, "ubuntu-22.04.5-live-server-amd64.iso")
	if !ok || got != "aaaa" {
		t.Errorf("got %q ok=%v", got, ok)
	}
}

func TestSHA256OfFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := sha256OfFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(sha256.New().Sum(nil)) // empty hash
	_ = want
	// Easier: compute expected directly.
	h := sha256.Sum256([]byte("hello"))
	if got != hex.EncodeToString(h[:]) {
		t.Errorf("hash mismatch: %s", got)
	}
}

func TestSHA256OfFile_MissingFile(t *testing.T) {
	if _, err := sha256OfFile("/this/file/cannot/exist"); err == nil {
		t.Error("expected open error")
	}
}

// --- Fetch end-to-end -------------------------------------------------------

func TestISOFetcher_Fetch_DownloadsAndCaches(t *testing.T) {
	srv, _, body := fakeISOServer(t)
	f := patchedFetcher(t, srv)
	path, err := f.Fetch()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded bytes mismatch")
	}

	// Second Fetch should hit cache (no .partial side-effects).
	path2, err := f.Fetch()
	if err != nil {
		t.Fatal(err)
	}
	if path != path2 {
		t.Errorf("cached path differs: %q vs %q", path, path2)
	}
}

func TestISOFetcher_Fetch_RedownloadsOnHashMismatch(t *testing.T) {
	srv, _, body := fakeISOServer(t)
	f := patchedFetcher(t, srv)
	path, err := f.Fetch()
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the cached file. Next Fetch should redownload.
	if err := os.WriteFile(path, []byte("CORRUPTED"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(); err != nil {
		t.Fatalf("Fetch on corrupted cache: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) {
		t.Error("corrupted cache wasn't replaced")
	}
}

func TestISOFetcher_Fetch_SHA256SUMSMissesFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/22.04/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		// Empty manifest — file isn't listed.
		_, _ = w.Write([]byte("# nothing\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := patchedFetcher(t, srv)
	_, err := f.Fetch()
	if err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Errorf("expected does-not-list error, got %v", err)
	}
}

func TestISOFetcher_Fetch_SHA256SUMSDownloadFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/22.04/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := patchedFetcher(t, srv)
	_, err := f.Fetch()
	if err == nil || !strings.Contains(err.Error(), "fetch SHA256SUMS") {
		t.Errorf("expected fetch error, got %v", err)
	}
}

func TestISOFetcher_Fetch_ISODownloadFails(t *testing.T) {
	body := []byte("ignored")
	sum := sha256.Sum256(body)
	mux := http.NewServeMux()
	mux.HandleFunc("/22.04/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s *ubuntu-22.04.5-live-server-amd64.iso\n", hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("/22.04/ubuntu-22.04.5-live-server-amd64.iso", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := patchedFetcher(t, srv)
	_, err := f.Fetch()
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 error, got %v", err)
	}
}

func TestISOFetcher_Fetch_HashMismatchOnFreshDownload(t *testing.T) {
	// Manifest claims a wrong hash; fresh download then fails verification.
	body := []byte("real bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/22.04/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "deadbeef *ubuntu-22.04.5-live-server-amd64.iso\n")
	})
	mux.HandleFunc("/22.04/ubuntu-22.04.5-live-server-amd64.iso", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := patchedFetcher(t, srv)
	path, err := f.Fetch()
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected mismatch error, got %v", err)
	}
	// Corrupted file removed.
	if path != "" {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Error("corrupted download should be removed")
		}
	}
}

func TestISOFetcher_DefaultsApplied(t *testing.T) {
	// Override homedir to confirm CacheDir default lands under it.
	origHome := userHomeDir
	defer func() { userHomeDir = origHome }()
	tmp := t.TempDir()
	userHomeDir = func() (string, error) { return tmp, nil }

	// Stub the real arch to avoid the unsupported-arch branch in CI
	// running on something exotic.
	f := &ISOFetcher{Arch: "amd64", HTTPClient: failingClient()}
	_, _ = f.Fetch() // expected to fail at SHA256SUMS — we don't care
	if !strings.HasPrefix(f.CacheDir, tmp) {
		t.Errorf("CacheDir = %q, want under %q", f.CacheDir, tmp)
	}
	if f.Version != DefaultUbuntuVersion {
		t.Errorf("Version not defaulted: %q", f.Version)
	}
}

func TestISOFetcher_HomeDirError(t *testing.T) {
	orig := userHomeDir
	defer func() { userHomeDir = orig }()
	userHomeDir = func() (string, error) { return "", errors.New("boom") }
	f := &ISOFetcher{Arch: "amd64", HTTPClient: failingClient()}
	if _, err := f.Fetch(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected home-dir error, got %v", err)
	}
}

func TestISOFetcher_DetectArchFails(t *testing.T) {
	// This test only matters on hosts where detectArch returns an
	// error — i.e. neither amd64 nor arm64. Skip on the common archs
	// since we can't fault the arch lookup without compile-time tags.
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		t.Skip("detectArch succeeds on this host")
	}
	f := &ISOFetcher{}
	if _, err := f.Fetch(); err == nil {
		t.Error("expected detectArch error")
	}
}

func TestISOFetcher_Arm64URL(t *testing.T) {
	f := &ISOFetcher{Version: "22.04.5", Arch: "arm64"}
	url := f.isoURL("ubuntu-22.04.5-live-server-arm64.iso")
	if !strings.HasPrefix(url, "https://cdimage.ubuntu.com/") {
		t.Errorf("arm64 should use cdimage host: %s", url)
	}
	// Sums URL also goes through cdimage.
	// Test indirectly through fetchSHA256SUMS hitting a stub if we
	// ever need full coverage; for now just validate URL shape.
}

// failingClient returns a client whose every request fails — useful
// for "default args still get filled in even when Fetch errors" tests.
func failingClient() *http.Client {
	return &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		}),
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
