package command

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gs3/internal/s3fake"
)

// harness runs gs3 in-process against an in-memory S3 server.
type harness struct {
	t    *testing.T
	fake *s3fake.Server
	dir  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := s3fake.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	t.Chdir(dir)
	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL":            srv.URL,
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_DEFAULT_REGION":          "us-east-1",
		"AWS_CONFIG_FILE":             filepath.Join(dir, "no-config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "no-credentials"),
		"AWS_EC2_METADATA_DISABLED":   "true",
		// Fail fast: these tests inject errors on purpose.
		"AWS_MAX_ATTEMPTS": "2",
		"TZ":               "UTC",
	} {
		t.Setenv(k, v)
	}
	return &harness{t: t, fake: fake, dir: dir}
}

// run executes `gs3 <args>` and returns the exit code and both streams.
func (h *harness) run(args ...string) (rc int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	rc = Main(append([]string{"gs3"}, args...), IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	return rc, out.String(), errOut.String()
}

func (h *harness) write(rel string, data []byte) string {
	h.t.Helper()
	path := filepath.Join(h.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) object(bucket, key string) (s3fake.ObjectInfo, bool) {
	for _, o := range h.fake.Objects(bucket) {
		if o.Key == key {
			return o, true
		}
	}
	return s3fake.ObjectInfo{}, false
}

// ops returns the names of the operations the server received.
func (h *harness) ops() map[string]int {
	counts := map[string]int{}
	for _, r := range h.fake.Requests() {
		counts[r.Op]++
	}
	return counts
}

func TestMultipartRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.fake.CreateBucket("bkt", time.Time{})
	t.Setenv("GS3_MULTIPART_THRESHOLD", "5MB")
	t.Setenv("GS3_MULTIPART_CHUNKSIZE", "5MB")

	data := randomBytes(t, 12<<20+123)
	h.write("big.bin", data)

	if rc, _, stderr := h.run("s3", "cp", "big.bin", "s3://bkt/big.bin", "--only-show-errors"); rc != 0 {
		t.Fatalf("upload: rc=%d stderr=%s", rc, stderr)
	}
	obj, ok := h.object("bkt", "big.bin")
	if !ok || !bytes.Equal(obj.Data, data) {
		t.Fatal("uploaded object differs from the source file")
	}
	if !strings.HasSuffix(obj.ETag, "-3") {
		t.Errorf("ETag %q: expected a 3-part multipart upload", obj.ETag)
	}
	if n := h.fake.PendingUploads(); n != 0 {
		t.Errorf("%d multipart uploads left open", n)
	}

	modified := time.Date(2024, 3, 5, 10, 20, 30, 0, time.UTC)
	h.fake.PutObject("bkt", "remote.bin", data, modified, nil)
	if rc, _, stderr := h.run("s3", "cp", "s3://bkt/remote.bin", "out/remote.bin", "--only-show-errors"); rc != 0 {
		t.Fatalf("download: rc=%d stderr=%s", rc, stderr)
	}
	got, err := os.ReadFile(filepath.Join(h.dir, "out/remote.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("downloaded file differs from the object")
	}
	st, _ := os.Stat(filepath.Join(h.dir, "out/remote.bin"))
	if !st.ModTime().Equal(modified) {
		t.Errorf("mtime = %v, want the object's LastModified %v", st.ModTime().UTC(), modified)
	}
	if h.ops()["GetObject"] < 3 {
		t.Errorf("expected ranged GETs for a large download, got %d GetObject", h.ops()["GetObject"])
	}
	entries, _ := os.ReadDir(filepath.Join(h.dir, "out"))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

// Requests must look like botocore 1.33's: Content-MD5 on uploads, and none
// of the newer SDK checksum headers that S3-compatible stores may reject.
func TestRequestsCarryNoNewChecksumHeaders(t *testing.T) {
	h := newHarness(t)
	h.fake.CreateBucket("bkt", time.Time{})
	h.fake.RequireDeleteMD5 = true
	h.write("d/a.txt", []byte("hello"))
	h.write("d/b.txt", []byte("world"))

	steps := [][]string{
		{"s3", "cp", "d", "s3://bkt/p", "--recursive"},
		{"s3", "ls", "s3://bkt/p/"},
		{"s3", "cp", "s3://bkt/p/a.txt", "s3://bkt/q/a.txt"},
		{"s3", "cp", "s3://bkt/p/a.txt", "down.txt"},
		{"s3", "rm", "s3://bkt/p", "--recursive"},
	}
	for _, args := range steps {
		if rc, _, stderr := h.run(args...); rc != 0 {
			t.Fatalf("%v: rc=%d stderr=%s", args, rc, stderr)
		}
	}
	for _, r := range h.fake.Requests() {
		for name := range r.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-amz-checksum-") || lower == "x-amz-sdk-checksum-algorithm" ||
				lower == "x-amz-trailer" || lower == "expect" {
				t.Errorf("%s request carries %s", r.Op, name)
			}
		}
		if strings.Contains(r.Query, "x-id=") {
			t.Errorf("%s request carries the SDK's x-id query parameter", r.Op)
		}
		switch r.Op {
		case "PutObject", "DeleteObjects":
			if r.Header.Get("Content-MD5") == "" {
				t.Errorf("%s request has no Content-MD5", r.Op)
			}
		}
	}
	if h.ops()["DeleteObjects"] != 1 || h.ops()["DeleteObject"] != 0 {
		t.Errorf("recursive rm should use one batch delete, got %v", h.ops())
	}
}

// A source listing that fails part-way must not be read as "the rest of the
// source does not exist": with --delete that would wipe the destination.
func TestSyncDeleteSurvivesFailedSourceListing(t *testing.T) {
	h := newHarness(t)
	h.fake.MaxKeys = 2 // force several listing pages
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		h.fake.PutObject("src", "p/"+key, []byte("x"), time.Time{}, nil)
		h.fake.PutObject("dst", "p/"+key, []byte("x"), time.Time{}, nil)
	}

	var mu sync.Mutex
	pages := 0
	h.fake.Fault = func(r s3fake.Request) *s3fake.Fault {
		if r.Op != "ListObjectsV2" || !strings.HasPrefix(r.Path, "/src") {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		pages++
		if pages >= 2 {
			return &s3fake.Fault{Status: 403, Code: "AccessDenied", Message: "Access Denied"}
		}
		return nil
	}

	rc, _, stderr := h.run("s3", "sync", "s3://src/p/", "s3://dst/p/", "--delete")
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	if !strings.Contains(stderr, "fatal error: An error occurred (AccessDenied) when calling the ListObjectsV2 operation") {
		t.Errorf("stderr = %q, want a fatal ListObjectsV2 error", stderr)
	}
	if got := len(h.fake.Objects("dst")); got != 6 {
		t.Errorf("destination has %d objects left, want all 6: a failed listing deleted files", got)
	}
}

func TestFailedMultipartUploadIsAborted(t *testing.T) {
	h := newHarness(t)
	h.fake.CreateBucket("bkt", time.Time{})
	t.Setenv("GS3_MULTIPART_THRESHOLD", "5MB")
	t.Setenv("GS3_MULTIPART_CHUNKSIZE", "5MB")
	h.write("big.bin", randomBytes(t, 11<<20))

	h.fake.Fault = func(r s3fake.Request) *s3fake.Fault {
		if r.Op == "UploadPart" && strings.Contains(r.Query, "partNumber=2") {
			return &s3fake.Fault{Status: 500, Code: "InternalError"}
		}
		return nil
	}
	rc, _, stderr := h.run("s3", "cp", "big.bin", "s3://bkt/big.bin")
	if rc != 1 || !strings.Contains(stderr, "upload failed: ./big.bin to s3://bkt/big.bin") {
		t.Errorf("rc=%d stderr=%q, want a failed upload", rc, stderr)
	}
	if n := h.fake.PendingUploads(); n != 0 {
		t.Errorf("%d multipart uploads left open after the failure", n)
	}
	if _, ok := h.object("bkt", "big.bin"); ok {
		t.Error("a partial object was created")
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	h := newHarness(t)
	h.fake.CreateBucket("bkt", time.Time{})
	t.Setenv("AWS_MAX_ATTEMPTS", "4")
	h.write("a.txt", []byte("hello"))

	var mu sync.Mutex
	failures := 0
	h.fake.Fault = func(r s3fake.Request) *s3fake.Fault {
		mu.Lock()
		defer mu.Unlock()
		if r.Op == "PutObject" && failures < 2 {
			failures++
			return &s3fake.Fault{Status: 503, Code: "SlowDown"}
		}
		return nil
	}
	if rc, _, stderr := h.run("s3", "cp", "a.txt", "s3://bkt/a.txt"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, stderr)
	}
	if obj, ok := h.object("bkt", "a.txt"); !ok || string(obj.Data) != "hello" {
		t.Error("object missing or wrong after retries")
	}
}

// A response body that breaks off is resumed from where it stopped.
func TestDownloadResumesTruncatedBody(t *testing.T) {
	h := newHarness(t)
	data := randomBytes(t, 3<<20)
	h.fake.PutObject("bkt", "f.bin", data, time.Time{}, nil)

	var mu sync.Mutex
	truncated := false
	h.fake.Fault = func(r s3fake.Request) *s3fake.Fault {
		mu.Lock()
		defer mu.Unlock()
		if r.Op == "GetObject" && !truncated {
			truncated = true
			return &s3fake.Fault{TruncateBody: 1 << 20}
		}
		return nil
	}
	if rc, _, stderr := h.run("s3", "cp", "s3://bkt/f.bin", "f.bin"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, stderr)
	}
	got, _ := os.ReadFile(filepath.Join(h.dir, "f.bin"))
	if !bytes.Equal(got, data) {
		t.Error("resumed download differs from the object")
	}
	ranged := false
	for _, r := range h.fake.Requests() {
		if r.Op == "GetObject" && strings.HasPrefix(r.Header.Get("Range"), "bytes=1048576-") {
			ranged = true
		}
	}
	if !ranged {
		t.Error("expected the retry to request only the missing bytes")
	}
}

// Stores without batch delete or UploadPartCopy still work, through the
// per-key and single-request fallbacks.
func TestFallbacksForUnsupportedOperations(t *testing.T) {
	h := newHarness(t)
	t.Setenv("GS3_MULTIPART_THRESHOLD", "5MB")
	t.Setenv("GS3_MULTIPART_CHUNKSIZE", "5MB")
	data := randomBytes(t, 6<<20)
	h.fake.PutObject("bkt", "src/big", data, time.Time{}, http.Header{"Content-Type": {"application/x-custom"}})
	h.fake.PutObject("bkt", "src/small", []byte("s"), time.Time{}, nil)
	h.fake.DisableUploadPartCopy = true
	h.fake.Fault = func(r s3fake.Request) *s3fake.Fault {
		if r.Op == "DeleteObjects" {
			return &s3fake.Fault{Status: 501, Code: "NotImplemented"}
		}
		return nil
	}

	if rc, _, stderr := h.run("s3", "cp", "s3://bkt/src/", "s3://bkt/dst/", "--recursive"); rc != 0 {
		t.Fatalf("copy: rc=%d stderr=%s", rc, stderr)
	}
	if obj, ok := h.object("bkt", "dst/big"); !ok || !bytes.Equal(obj.Data, data) {
		t.Error("large object was not copied through the CopyObject fallback")
	}
	if n := h.fake.PendingUploads(); n != 0 {
		t.Errorf("%d multipart uploads left open after falling back", n)
	}

	if rc, _, stderr := h.run("s3", "rm", "s3://bkt/src/", "--recursive"); rc != 0 {
		t.Fatalf("rm: rc=%d stderr=%s", rc, stderr)
	}
	for _, o := range h.fake.Objects("bkt") {
		if strings.HasPrefix(o.Key, "src/") {
			t.Errorf("%s was not deleted by the per-key fallback", o.Key)
		}
	}
}

// A multipart copy keeps the source's Content-Type and metadata, which AWS
// CLI 1.x loses.
func TestMultipartCopyKeepsMetadata(t *testing.T) {
	h := newHarness(t)
	t.Setenv("GS3_MULTIPART_THRESHOLD", "5MB")
	t.Setenv("GS3_MULTIPART_CHUNKSIZE", "5MB")
	data := randomBytes(t, 11<<20)
	h.fake.PutObject("bkt", "src.bin", data, time.Time{}, http.Header{
		"Content-Type": {"application/x-custom"}, "X-Amz-Meta-Owner": {"me"}, "Cache-Control": {"max-age=5"},
	})
	if rc, _, stderr := h.run("s3", "cp", "s3://bkt/src.bin", "s3://bkt/dst.bin"); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, stderr)
	}
	obj, ok := h.object("bkt", "dst.bin")
	if !ok || !bytes.Equal(obj.Data, data) {
		t.Fatal("copied object differs from the source")
	}
	if h.ops()["UploadPartCopy"] < 2 {
		t.Errorf("expected a multipart copy, got %v", h.ops())
	}
	for name, want := range map[string]string{"Content-Type": "application/x-custom", "X-Amz-Meta-Owner": "me", "Cache-Control": "max-age=5"} {
		if got := obj.Header.Get(name); got != want {
			t.Errorf("%s = %q after a multipart copy, want %q", name, got, want)
		}
	}
}

func TestStreams(t *testing.T) {
	h := newHarness(t)
	h.fake.CreateBucket("bkt", time.Time{})
	t.Setenv("GS3_MULTIPART_CHUNKSIZE", "5MB")
	t.Setenv("GS3_MULTIPART_THRESHOLD", "5MB")
	data := randomBytes(t, 12<<20+7)

	var out, errOut bytes.Buffer
	rc := Main([]string{"gs3", "s3", "cp", "-", "s3://bkt/stream.bin"},
		IO{Stdin: bytes.NewReader(data), Stdout: &out, Stderr: &errOut})
	if rc != 0 {
		t.Fatalf("stream upload: rc=%d stderr=%s", rc, errOut.String())
	}
	if obj, ok := h.object("bkt", "stream.bin"); !ok || !bytes.Equal(obj.Data, data) {
		t.Fatal("object uploaded from stdin differs from the input")
	}
	if n := h.fake.PendingUploads(); n != 0 {
		t.Errorf("%d multipart uploads left open", n)
	}

	out.Reset()
	rc = Main([]string{"gs3", "s3", "cp", "s3://bkt/stream.bin", "-"},
		IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	if rc != 0 {
		t.Fatalf("stream download: rc=%d stderr=%s", rc, errOut.String())
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("bytes written to stdout differ from the object")
	}
}

func TestPresign(t *testing.T) {
	h := newHarness(t)
	h.fake.PutObject("bkt", "dir/a b.txt", []byte("presigned"), time.Time{}, nil)

	rc, stdout, stderr := h.run("s3", "presign", "s3://bkt/dir/a b.txt", "--expires-in", "120")
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, stderr)
	}
	url := strings.TrimSpace(stdout)
	for _, want := range []string{"/bkt/dir/a%20b.txt?", "X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Expires=120", "X-Amz-SignedHeaders=host", "X-Amz-Signature="} {
		if !strings.Contains(url, want) {
			t.Errorf("presigned URL %q lacks %q", url, want)
		}
	}
	if strings.Contains(url, "x-id=") {
		t.Errorf("presigned URL %q carries the SDK's x-id parameter", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET on the presigned URL returned %d", resp.StatusCode)
	}
}

func TestSimpleCommands(t *testing.T) {
	h := newHarness(t)

	if rc, stdout, _ := h.run("s3", "mb", "s3://newbkt"); rc != 0 || stdout != "make_bucket: newbkt\n" {
		t.Errorf("mb: rc=%d stdout=%q", rc, stdout)
	}
	h.fake.PutObject("newbkt", "dir/a.txt", []byte("12345"), time.Date(2024, 3, 5, 10, 20, 30, 0, time.UTC), nil)

	rc, stdout, _ := h.run("s3", "ls", "s3://newbkt/")
	if want := "                           PRE dir/\n"; rc != 0 || stdout != want {
		t.Errorf("ls: rc=%d stdout=%q, want %q", rc, stdout, want)
	}
	rc, stdout, _ = h.run("s3", "ls", "s3://newbkt/dir/", "--summarize")
	if want := "2024-03-05 10:20:30          5 a.txt\n\nTotal Objects: 1\n   Total Size: 5\n"; rc != 0 || stdout != want {
		t.Errorf("ls --summarize: rc=%d stdout=%q, want %q", rc, stdout, want)
	}
	if rc, _, _ := h.run("s3", "ls", "s3://newbkt/missing"); rc != 1 {
		t.Errorf("ls of a prefix that matches nothing: rc=%d, want 1", rc)
	}
	if rc, _, _ := h.run("s3", "website", "s3://newbkt", "--index-document", "index.html"); rc != 0 {
		t.Errorf("website: rc=%d", rc)
	}
	if !bytes.Contains(h.fake.BucketWebsite("newbkt"), []byte("<Suffix>index.html</Suffix>")) {
		t.Errorf("website configuration not stored: %s", h.fake.BucketWebsite("newbkt"))
	}
	if rc, _, stderr := h.run("s3", "rb", "s3://newbkt"); rc != 1 || !strings.Contains(stderr, "BucketNotEmpty") {
		t.Errorf("rb on a non-empty bucket: rc=%d stderr=%q", rc, stderr)
	}
	if rc, stdout, _ := h.run("s3", "rb", "s3://newbkt", "--force"); rc != 0 || !strings.HasSuffix(stdout, "remove_bucket: newbkt\n") {
		t.Errorf("rb --force: rc=%d stdout=%q", rc, stdout)
	}
	if rc, _, stderr := h.run("s3", "cp", "a", "b"); rc != 255 || !strings.Contains(stderr, "Error: Invalid argument type") {
		t.Errorf("local-to-local cp: rc=%d stderr=%q", rc, stderr)
	}
	if rc, _, stderr := h.run("s3", "ls", "--page-size"); rc != 2 || !strings.Contains(stderr, "gs3: error: argument --page-size: expected one argument") {
		t.Errorf("usage error: rc=%d stderr=%q", rc, stderr)
	}
}
