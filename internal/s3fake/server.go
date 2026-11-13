// Package s3fake is an in-memory S3 server for tests.
//
// It implements the subset of the S3 REST API that `aws s3` uses (path-style
// addressing only), records every request it receives so tests can assert on
// the wire behaviour, and lets tests inject faults. Request signatures are not
// verified.
//
// It is used three ways: in-process by Go tests (httptest), as a standalone
// process (tools/s3fake) that both gs3 and the reference AWS CLI container
// talk to during differential tests, and as a fault-injection target.
package s3fake

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is one recorded request.
type Request struct {
	// Op is the S3 operation name, e.g. "PutObject".
	Op     string      `json:"op"`
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Query  string      `json:"query"`
	Header http.Header `json:"header"`
	// BodyLen is the number of body bytes received.
	BodyLen int64 `json:"body_len"`
	Status  int   `json:"status"`
}

// Fault makes the server misbehave for one request.
type Fault struct {
	// Status and Code produce an S3 error response instead of handling the
	// request.
	Status  int
	Code    string
	Message string
	// Delay is applied before the request is handled.
	Delay time.Duration
	// TruncateBody cuts a GetObject response body after this many bytes and
	// drops the connection.
	TruncateBody int64
	// Reset drops the connection without answering.
	Reset bool
}

type object struct {
	data     []byte
	etag     string
	modified time.Time
	// header holds the stored system and user metadata headers.
	header http.Header
}

type bucket struct {
	created time.Time
	objects map[string]*object
	sorted  []string // cached sorted keys; nil when stale
	website []byte
	// location is the LocationConstraint given at creation.
	location string
}

type upload struct {
	bucket, key string
	header      http.Header
	parts       map[int]*object
	started     time.Time
}

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	uploads  map[string]*upload
	requests []Request
	nextID   int

	// Now supplies object modification times.
	Now func() time.Time
	// MinPartSize is the smallest allowed non-final multipart part.
	MinPartSize int64
	// MaxKeys caps the page size of listings (S3 uses 1000).
	MaxKeys int
	// RequireDeleteMD5 rejects DeleteObjects requests without Content-MD5,
	// as older S3-compatible stores do.
	RequireDeleteMD5 bool
	// DisableUploadPartCopy answers UploadPartCopy with 501 NotImplemented.
	DisableUploadPartCopy bool
	// Fault, when set, is consulted for every request.
	Fault func(r Request) *Fault
}

// New returns an empty server.
func New() *Server {
	return &Server{
		buckets:     map[string]*bucket{},
		uploads:     map[string]*upload{},
		Now:         time.Now,
		MinPartSize: 5 << 20,
		MaxKeys:     1000,
	}
}

// Requests returns a copy of the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// ResetRequests forgets the recorded requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// Reset removes all buckets, uploads and recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = map[string]*bucket{}
	s.uploads = map[string]*upload{}
	s.requests = nil
}

// CreateBucket adds a bucket directly. A zero created time means now.
func (s *Server) CreateBucket(name string, created time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if created.IsZero() {
		created = s.Now()
	}
	if b, ok := s.buckets[name]; ok {
		b.created = created.UTC()
		return
	}
	s.buckets[name] = &bucket{created: created.UTC(), objects: map[string]*object{}}
}

// PutObject stores an object directly, bypassing HTTP.
func (s *Server) PutObject(bucketName, key string, data []byte, modified time.Time, header http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		b = &bucket{created: s.Now().UTC(), objects: map[string]*object{}}
		s.buckets[bucketName] = b
	}
	if modified.IsZero() {
		modified = s.Now()
	}
	if header == nil {
		header = http.Header{}
	}
	b.put(key, &object{data: data, etag: md5Hex(data), modified: modified.UTC(), header: header})
}

// ObjectInfo describes a stored object for assertions.
type ObjectInfo struct {
	Key      string
	Data     []byte
	ETag     string
	Modified time.Time
	Header   http.Header
}

// Objects returns the objects of a bucket in key order.
func (s *Server) Objects(bucketName string) []ObjectInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return nil
	}
	var out []ObjectInfo
	for _, k := range b.keys() {
		o := b.objects[k]
		out = append(out, ObjectInfo{Key: k, Data: o.data, ETag: o.etag, Modified: o.modified, Header: o.header.Clone()})
	}
	return out
}

// Buckets returns the bucket names in order.
func (s *Server) Buckets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// PendingUploads returns the number of multipart uploads neither completed
// nor aborted.
func (s *Server) PendingUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// BucketLocation returns the LocationConstraint a bucket was created with.
func (s *Server) BucketLocation(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		return b.location
	}
	return ""
}

// BucketWebsite returns the stored website configuration XML.
func (s *Server) BucketWebsite(name string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		return b.website
	}
	return nil
}

func (b *bucket) put(key string, o *object) {
	if _, exists := b.objects[key]; !exists {
		b.sorted = nil
	}
	b.objects[key] = o
}

func (b *bucket) remove(key string) {
	if _, exists := b.objects[key]; exists {
		delete(b.objects, key)
		b.sorted = nil
	}
}

func (b *bucket) keys() []string {
	if b.sorted == nil {
		b.sorted = make([]string, 0, len(b.objects))
		for k := range b.objects {
			b.sorted = append(b.sorted, k)
		}
		sort.Strings(b.sorted)
	}
	return b.sorted
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// storedHeaders are the request headers kept with an object and returned on
// GET/HEAD.
var storedHeaders = []string{
	"Content-Type", "Cache-Control", "Content-Disposition", "Content-Encoding",
	"Content-Language", "Expires", "X-Amz-Storage-Class",
	"X-Amz-Website-Redirect-Location", "X-Amz-Server-Side-Encryption",
	"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id",
	"X-Amz-Server-Side-Encryption-Customer-Algorithm",
	"X-Amz-Server-Side-Encryption-Customer-Key-Md5",
	// Not returned by real S3, but kept so tests can assert on them.
	"X-Amz-Acl", "X-Amz-Grant-Read", "X-Amz-Grant-Read-Acp",
	"X-Amz-Grant-Write-Acp", "X-Amz-Grant-Full-Control",
}

func objectHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, name := range storedHeaders {
		if v := h.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	for name, values := range h {
		if strings.HasPrefix(name, "X-Amz-Meta-") && len(values) > 0 {
			out.Set(name, values[0])
		}
	}
	return out
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Header: r.Header.Clone(), BodyLen: int64(len(body)),
	}
	bucketName, key := splitPath(r.URL.Path)
	rec.Op = operationName(r, bucketName, key)

	rw := &statusWriter{ResponseWriter: w, status: 200}
	defer func() {
		rec.Status = rw.status
		s.mu.Lock()
		s.requests = append(s.requests, rec)
		s.mu.Unlock()
	}()

	var truncate int64
	if s.Fault != nil {
		if f := s.Fault(rec); f != nil {
			if f.Delay > 0 {
				time.Sleep(f.Delay)
			}
			if f.Reset {
				dropConnection(rw)
				rw.status = 0
				return
			}
			if f.Status != 0 {
				writeError(rw, f.Status, f.Code, f.Message)
				return
			}
			truncate = f.TruncateBody
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle(rw, r, rec.Op, bucketName, key, body, truncate)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func dropConnection(w http.ResponseWriter) {
	if hj, ok := w.(*statusWriter).ResponseWriter.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

func splitPath(path string) (bucketName, key string) {
	path = strings.TrimPrefix(path, "/")
	bucketName, key, _ = strings.Cut(path, "/")
	return bucketName, key
}

func operationName(r *http.Request, bucketName, key string) string {
	q := r.URL.Query()
	has := func(name string) bool { _, ok := q[name]; return ok }
	switch {
	case bucketName == "":
		return "ListBuckets"
	case key == "":
		switch r.Method {
		case http.MethodGet:
			switch {
			case has("uploads"):
				return "ListMultipartUploads"
			case has("website"):
				return "GetBucketWebsite"
			case has("location"):
				return "GetBucketLocation"
			case q.Get("list-type") == "2":
				return "ListObjectsV2"
			}
			return "ListObjects"
		case http.MethodPut:
			if has("website") {
				return "PutBucketWebsite"
			}
			return "CreateBucket"
		case http.MethodDelete:
			return "DeleteBucket"
		case http.MethodHead:
			return "HeadBucket"
		case http.MethodPost:
			if has("delete") {
				return "DeleteObjects"
			}
		}
	default:
		switch r.Method {
		case http.MethodGet:
			return "GetObject"
		case http.MethodHead:
			return "HeadObject"
		case http.MethodPut:
			copying := r.Header.Get("X-Amz-Copy-Source") != ""
			switch {
			case has("uploadId") && copying:
				return "UploadPartCopy"
			case has("uploadId"):
				return "UploadPart"
			case copying:
				return "CopyObject"
			}
			return "PutObject"
		case http.MethodDelete:
			if has("uploadId") {
				return "AbortMultipartUpload"
			}
			return "DeleteObject"
		case http.MethodPost:
			switch {
			case has("uploads"):
				return "CreateMultipartUpload"
			case has("uploadId"):
				return "CompleteMultipartUpload"
			}
		}
	}
	return "Unknown"
}

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

var errorMessages = map[string]string{
	"NoSuchBucket":              "The specified bucket does not exist",
	"NoSuchKey":                 "The specified key does not exist.",
	"NoSuchUpload":              "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
	"BucketNotEmpty":            "The bucket you tried to delete is not empty",
	"BucketAlreadyOwnedByYou":   "Your previous request to create the named bucket succeeded and you already own it.",
	"BadDigest":                 "The Content-MD5 you specified did not match what we received.",
	"InvalidRange":              "The requested range is not satisfiable",
	"PreconditionFailed":        "At least one of the pre-conditions you specified did not hold",
	"EntityTooSmall":            "Your proposed upload is smaller than the minimum allowed size",
	"InvalidPart":               "One or more of the specified parts could not be found.  The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.",
	"NotImplemented":            "A header you provided implies functionality that is not implemented",
	"SlowDown":                  "Please reduce your request rate.",
	"InternalError":             "We encountered an internal error. Please try again.",
	"ServiceUnavailable":        "Service is unable to handle request.",
	"MalformedXML":              "The XML you provided was not well-formed or did not validate against our published schema",
	"InvalidRequest":            "Invalid Request",
	"XAmzContentSHA256Mismatch": "The provided 'x-amz-content-sha256' header does not match what was computed.",
	"MethodNotAllowed":          "The specified method is not allowed against this resource.",
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	if message == "" {
		message = errorMessages[code]
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", "FAKEREQUESTID")
	w.WriteHeader(status)
	if status == http.StatusNotModified {
		return
	}
	out, _ := xml.Marshal(errorBody{Code: code, Message: message, RequestID: "FAKEREQUESTID", HostID: "fakehost"})
	w.Write([]byte(xml.Header))
	w.Write(out)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	out, err := xml.Marshal(v)
	if err != nil {
		writeError(w, 500, "InternalError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", "FAKEREQUESTID")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(out)
}

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

const timeFormat = "2006-01-02T15:04:05.000Z"

func (s *Server) handle(w http.ResponseWriter, r *http.Request, op, bucketName, key string, body []byte, truncate int64) {
	q := r.URL.Query()

	if op == "ListBuckets" {
		s.listBuckets(w)
		return
	}
	if op == "CreateBucket" {
		if _, exists := s.buckets[bucketName]; exists {
			writeError(w, 409, "BucketAlreadyOwnedByYou", "")
			return
		}
		b := &bucket{created: s.Now().UTC(), objects: map[string]*object{}}
		var cfg struct {
			LocationConstraint string `xml:"LocationConstraint"`
		}
		if len(body) > 0 && xml.Unmarshal(body, &cfg) == nil {
			b.location = cfg.LocationConstraint
		}
		s.buckets[bucketName] = b
		w.Header().Set("Location", "/"+bucketName)
		w.WriteHeader(200)
		return
	}

	b, ok := s.buckets[bucketName]
	if !ok {
		if r.Method == http.MethodHead {
			w.WriteHeader(404)
			return
		}
		writeError(w, 404, "NoSuchBucket", "")
		return
	}

	switch op {
	case "HeadBucket":
		w.WriteHeader(200)
	case "DeleteBucket":
		if len(b.objects) > 0 {
			writeError(w, 409, "BucketNotEmpty", "")
			return
		}
		delete(s.buckets, bucketName)
		w.WriteHeader(204)
	case "PutBucketWebsite":
		b.website = body
		w.WriteHeader(200)
	case "GetBucketWebsite":
		if b.website == nil {
			writeError(w, 404, "NoSuchWebsiteConfiguration", "The specified bucket does not have a website configuration")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Write(b.website)
	case "GetBucketLocation":
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Xmlns   string   `xml:"xmlns,attr"`
			Value   string   `xml:",chardata"`
		}{Xmlns: xmlns, Value: b.location})
	case "ListObjectsV2", "ListObjects":
		s.listObjects(w, q, bucketName, b, op == "ListObjectsV2")
	case "ListMultipartUploads":
		s.listUploads(w, bucketName)
	case "DeleteObjects":
		s.deleteObjects(w, r, b, body)
	case "PutObject":
		s.putObject(w, r, b, key, body)
	case "CopyObject":
		s.copyObject(w, r, b, key)
	case "GetObject", "HeadObject":
		s.getObject(w, r, b, key, op == "HeadObject", truncate)
	case "DeleteObject":
		b.remove(key)
		w.WriteHeader(204)
	case "CreateMultipartUpload":
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.uploads[id] = &upload{bucket: bucketName, key: key, header: objectHeaders(r.Header), parts: map[int]*object{}, started: s.Now().UTC()}
		writeXML(w, 200, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			Xmlns    string   `xml:"xmlns,attr"`
			Bucket   string
			Key      string
			UploadID string `xml:"UploadId"`
		}{Xmlns: xmlns, Bucket: bucketName, Key: key, UploadID: id})
	case "UploadPart", "UploadPartCopy":
		s.uploadPart(w, r, q, op == "UploadPartCopy", body)
	case "CompleteMultipartUpload":
		s.completeUpload(w, q, b, bucketName, key, body)
	case "AbortMultipartUpload":
		id := q.Get("uploadId")
		if _, ok := s.uploads[id]; !ok {
			writeError(w, 404, "NoSuchUpload", "")
			return
		}
		delete(s.uploads, id)
		w.WriteHeader(204)
	default:
		writeError(w, 405, "MethodNotAllowed", "")
	}
}

func (s *Server) listBuckets(w http.ResponseWriter) {
	type bucketXML struct {
		Name         string
		CreationDate string
	}
	var names []string
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	list := make([]bucketXML, 0, len(names))
	for _, n := range names {
		list = append(list, bucketXML{Name: n, CreationDate: s.buckets[n].created.Format(timeFormat)})
	}
	writeXML(w, 200, struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Xmlns   string   `xml:"xmlns,attr"`
		Owner   struct {
			ID          string
			DisplayName string
		}
		Buckets struct {
			Bucket []bucketXML
		}
	}{Xmlns: xmlns, Owner: struct {
		ID          string
		DisplayName string
	}{"fakeowner", "fakeowner"}, Buckets: struct{ Bucket []bucketXML }{list}})
}

type contentXML struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type prefixXML struct {
	Prefix string
}

func (s *Server) listObjects(w http.ResponseWriter, q url.Values, bucketName string, b *bucket, v2 bool) {
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	maxKeys := s.MaxKeys
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, 400, "InvalidArgument", "Provided max-keys not an integer or within integer range")
			return
		}
		maxKeys = min(n, s.MaxKeys)
	}
	encode := func(v string) string { return v }
	if q.Get("encoding-type") == "url" {
		encode = s3URLEncode
	}

	// The marker is the last name returned by the previous page. When that
	// name was a common prefix, everything under it is skipped too.
	marker := q.Get("marker")
	if v2 {
		marker = q.Get("start-after")
		if token := q.Get("continuation-token"); token != "" {
			raw, err := base64.StdEncoding.DecodeString(token)
			if err != nil {
				writeError(w, 400, "InvalidArgument", "The continuation token provided is incorrect")
				return
			}
			marker = string(raw)
		}
	}

	keys := b.keys()
	start := sort.SearchStrings(keys, marker)
	if marker != "" {
		for start < len(keys) && (keys[start] == marker ||
			(delimiter != "" && strings.HasSuffix(marker, delimiter) && strings.HasPrefix(keys[start], marker))) {
			start++
		}
	}

	var contents []contentXML
	var prefixes []prefixXML
	truncated := false
	last := ""
	count := 0
	for i := start; i < len(keys); i++ {
		k := keys[i]
		if !strings.HasPrefix(k, prefix) {
			if k > prefix {
				break
			}
			continue
		}
		name := k
		isPrefix := false
		if delimiter != "" {
			if idx := strings.Index(k[len(prefix):], delimiter); idx >= 0 {
				name = k[:len(prefix)+idx+len(delimiter)]
				isPrefix = true
			}
		}
		if isPrefix && name == last {
			continue
		}
		if count >= maxKeys {
			truncated = true
			break
		}
		count++
		last = name
		if isPrefix {
			prefixes = append(prefixes, prefixXML{Prefix: encode(name)})
			continue
		}
		o := b.objects[k]
		class := o.header.Get("X-Amz-Storage-Class")
		if class == "" {
			class = "STANDARD"
		}
		contents = append(contents, contentXML{
			Key: encode(k), LastModified: o.modified.Format(timeFormat),
			ETag: `"` + o.etag + `"`, Size: int64(len(o.data)), StorageClass: class,
		})
	}

	result := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Xmlns                 string   `xml:"xmlns,attr"`
		Name                  string
		Prefix                string
		Marker                *string `xml:",omitempty"`
		NextMarker            string  `xml:",omitempty"`
		StartAfter            string  `xml:",omitempty"`
		ContinuationToken     string  `xml:",omitempty"`
		NextContinuationToken string  `xml:",omitempty"`
		KeyCount              *int    `xml:",omitempty"`
		MaxKeys               int
		Delimiter             string `xml:",omitempty"`
		EncodingType          string `xml:",omitempty"`
		IsTruncated           bool
		Contents              []contentXML
		CommonPrefixes        []prefixXML
	}{
		Xmlns: xmlns, Name: bucketName, Prefix: encode(prefix), MaxKeys: maxKeys,
		Delimiter: encode(delimiter), EncodingType: q.Get("encoding-type"),
		IsTruncated: truncated, Contents: contents, CommonPrefixes: prefixes,
	}
	if v2 {
		result.KeyCount = &count
		result.StartAfter = encode(q.Get("start-after"))
		result.ContinuationToken = q.Get("continuation-token")
		if truncated {
			result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(last))
		}
	} else {
		m := encode(q.Get("marker"))
		result.Marker = &m
		if truncated {
			result.NextMarker = encode(last)
		}
	}
	writeXML(w, 200, result)
}

// s3URLEncode encodes a key the way S3 does for encoding-type=url.
func s3URLEncode(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '*', c == '/':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) listUploads(w http.ResponseWriter, bucketName string) {
	type uploadXML struct {
		Key       string
		UploadID  string `xml:"UploadId"`
		Initiated string
	}
	var list []uploadXML
	for id, u := range s.uploads {
		if u.bucket == bucketName {
			list = append(list, uploadXML{Key: u.key, UploadID: id, Initiated: u.started.Format(timeFormat)})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	writeXML(w, 200, struct {
		XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
		Xmlns       string   `xml:"xmlns,attr"`
		Bucket      string
		IsTruncated bool
		Upload      []uploadXML
	}{Xmlns: xmlns, Bucket: bucketName, Upload: list})
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, b *bucket, body []byte) {
	if s.RequireDeleteMD5 && r.Header.Get("Content-MD5") == "" {
		writeError(w, 400, "InvalidRequest", "Missing required header for this request: Content-MD5")
		return
	}
	if !verifyBody(w, r, body) {
		return
	}
	var req struct {
		Quiet  bool `xml:"Quiet"`
		Object []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "MalformedXML", "")
		return
	}
	type deletedXML struct {
		Key string
	}
	var deleted []deletedXML
	for _, o := range req.Object {
		b.remove(o.Key)
		if !req.Quiet {
			deleted = append(deleted, deletedXML{Key: o.Key})
		}
	}
	writeXML(w, 200, struct {
		XMLName xml.Name `xml:"DeleteResult"`
		Xmlns   string   `xml:"xmlns,attr"`
		Deleted []deletedXML
	}{Xmlns: xmlns, Deleted: deleted})
}

// verifyBody checks the integrity headers a client sent, the way S3 does. It
// writes the error response and returns false on a mismatch.
func verifyBody(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if v := r.Header.Get("Content-MD5"); v != "" {
		sum := md5.Sum(body)
		if v != base64.StdEncoding.EncodeToString(sum[:]) {
			writeError(w, 400, "BadDigest", "")
			return false
		}
	}
	switch v := r.Header.Get("X-Amz-Content-Sha256"); {
	case v == "" || v == "UNSIGNED-PAYLOAD":
	case strings.HasPrefix(v, "STREAMING-"):
		// aws-chunked uploads: deliberately unsupported, as on several
		// S3-compatible stores, so that tests notice if gs3 ever sends one.
		writeError(w, 501, "NotImplemented", "")
		return false
	default:
		sum := sha256.Sum256(body)
		if v != hex.EncodeToString(sum[:]) {
			writeError(w, 400, "XAmzContentSHA256Mismatch", "")
			return false
		}
	}
	return true
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, b *bucket, key string, body []byte) {
	if !verifyBody(w, r, body) {
		return
	}
	o := &object{data: body, etag: md5Hex(body), modified: s.Now().UTC(), header: objectHeaders(r.Header)}
	b.put(key, o)
	w.Header().Set("ETag", `"`+o.etag+`"`)
	w.WriteHeader(200)
}

// copySource resolves the X-Amz-Copy-Source header.
func (s *Server) copySource(w http.ResponseWriter, r *http.Request) (*object, bool) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		writeError(w, 400, "InvalidArgument", "Invalid copy source object key")
		return nil, false
	}
	if i := strings.Index(src, "?"); i >= 0 {
		src = src[:i]
	}
	srcBucket, srcKey := splitPath(src)
	sb, ok := s.buckets[srcBucket]
	if !ok {
		writeError(w, 404, "NoSuchBucket", "")
		return nil, false
	}
	so, ok := sb.objects[srcKey]
	if !ok {
		writeError(w, 404, "NoSuchKey", "")
		return nil, false
	}
	return so, true
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, b *bucket, key string) {
	so, ok := s.copySource(w, r)
	if !ok {
		return
	}
	header := so.header.Clone()
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		header = objectHeaders(r.Header)
	} else {
		// Storage class, ACL and encryption always come from the request.
		for _, name := range []string{"X-Amz-Storage-Class", "X-Amz-Acl", "X-Amz-Server-Side-Encryption", "X-Amz-Website-Redirect-Location"} {
			if v := r.Header.Get(name); v != "" {
				header.Set(name, v)
			} else {
				header.Del(name)
			}
		}
	}
	o := &object{data: so.data, etag: so.etag, modified: s.Now().UTC(), header: header}
	b.put(key, o)
	writeXML(w, 200, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		LastModified string
		ETag         string
	}{LastModified: o.modified.Format(timeFormat), ETag: `"` + o.etag + `"`})
}

// parseRange parses "bytes=a-b" against an object of the given size.
func parseRange(spec string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(spec, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		n = min(n, size)
		return size - n, size - 1, size > 0
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, b *bucket, key string, head bool, truncate int64) {
	o, ok := b.objects[key]
	if !ok {
		if head {
			w.WriteHeader(404)
			return
		}
		writeError(w, 404, "NoSuchKey", "")
		return
	}
	quoted := `"` + o.etag + `"`
	if v := r.Header.Get("If-Match"); v != "" && v != quoted && v != o.etag {
		if head {
			w.WriteHeader(412)
			return
		}
		writeError(w, 412, "PreconditionFailed", "")
		return
	}

	h := w.Header()
	for name, values := range o.header {
		if strings.HasPrefix(name, "X-Amz-Acl") || strings.HasPrefix(name, "X-Amz-Grant-") {
			continue
		}
		if name == "X-Amz-Storage-Class" && values[0] == "STANDARD" {
			continue
		}
		h[name] = values
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "binary/octet-stream")
	}
	h.Set("ETag", quoted)
	h.Set("Last-Modified", o.modified.Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	h.Set("x-amz-request-id", "FAKEREQUESTID")

	data := o.data
	status := 200
	if spec := r.Header.Get("Range"); spec != "" {
		start, end, ok := parseRange(spec, int64(len(o.data)))
		if !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", len(o.data)))
			if head {
				w.WriteHeader(416)
				return
			}
			writeError(w, 416, "InvalidRange", "")
			return
		}
		data = o.data[start : end+1]
		status = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if head {
		return
	}
	if truncate > 0 && truncate < int64(len(data)) {
		w.Write(data[:truncate])
		if f, ok := w.(*statusWriter).ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		dropConnection(w)
		return
	}
	w.Write(data)
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, q url.Values, copying bool, body []byte) {
	u, ok := s.uploads[q.Get("uploadId")]
	if !ok {
		writeError(w, 404, "NoSuchUpload", "")
		return
	}
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		writeError(w, 400, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		return
	}

	if copying {
		if s.DisableUploadPartCopy {
			writeError(w, 501, "NotImplemented", "")
			return
		}
		so, ok := s.copySource(w, r)
		if !ok {
			return
		}
		data := so.data
		if spec := r.Header.Get("X-Amz-Copy-Source-Range"); spec != "" {
			start, end, ok := parseRange(spec, int64(len(so.data)))
			if !ok {
				writeError(w, 400, "InvalidArgument", "The x-amz-copy-source-range value must be of the form bytes=first-last")
				return
			}
			data = so.data[start : end+1]
		}
		part := &object{data: bytes.Clone(data), etag: md5Hex(data), modified: s.Now().UTC()}
		u.parts[n] = part
		writeXML(w, 200, struct {
			XMLName      xml.Name `xml:"CopyPartResult"`
			LastModified string
			ETag         string
		}{LastModified: part.modified.Format(timeFormat), ETag: `"` + part.etag + `"`})
		return
	}

	if !verifyBody(w, r, body) {
		return
	}
	part := &object{data: body, etag: md5Hex(body), modified: s.Now().UTC()}
	u.parts[n] = part
	w.Header().Set("ETag", `"`+part.etag+`"`)
	w.WriteHeader(200)
}

func (s *Server) completeUpload(w http.ResponseWriter, q url.Values, b *bucket, bucketName, key string, body []byte) {
	id := q.Get("uploadId")
	u, ok := s.uploads[id]
	if !ok {
		writeError(w, 404, "NoSuchUpload", "")
		return
	}
	var req struct {
		Part []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Part) == 0 {
		writeError(w, 400, "MalformedXML", "")
		return
	}
	var data []byte
	digests := md5.New()
	prev := 0
	for i, p := range req.Part {
		part, ok := u.parts[p.PartNumber]
		if !ok || strings.Trim(p.ETag, `"`) != part.etag {
			writeError(w, 400, "InvalidPart", "")
			return
		}
		if p.PartNumber <= prev {
			writeError(w, 400, "InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.")
			return
		}
		prev = p.PartNumber
		if i < len(req.Part)-1 && int64(len(part.data)) < s.MinPartSize {
			writeError(w, 400, "EntityTooSmall", "")
			return
		}
		data = append(data, part.data...)
		raw, _ := hex.DecodeString(part.etag)
		digests.Write(raw)
	}
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(digests.Sum(nil)), len(req.Part))
	b.put(key, &object{data: data, etag: etag, modified: s.Now().UTC(), header: u.header})
	delete(s.uploads, id)
	writeXML(w, 200, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		Xmlns    string   `xml:"xmlns,attr"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{Xmlns: xmlns, Location: "/" + bucketName + "/" + key, Bucket: bucketName, Key: key, ETag: `"` + etag + `"`})
}
