// Command s3fake runs the in-memory S3 server from internal/s3fake as a
// standalone process, so that gs3 and the reference AWS CLI container can be
// pointed at the same endpoint during differential tests.
//
// Besides the S3 API it serves a small control API under /_gs3fake/:
//
//	GET    /_gs3fake/requests   recorded requests as JSON
//	DELETE /_gs3fake/requests   forget recorded requests
//	GET    /_gs3fake/state      every bucket and object (size, md5, mtime, headers)
//	POST   /_gs3fake/seed       create objects from a JSON list
//	POST   /_gs3fake/reset      drop all buckets, uploads and requests
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gs3/internal/s3fake"
)

type seedObject struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	// Data is the literal content; when empty, Size bytes of a repeating
	// pattern derived from the key are generated instead.
	Data  string `json:"data"`
	Size  int    `json:"size"`
	MTime string `json:"mtime"` // RFC 3339; defaults to now
	// Headers are stored object headers, e.g. X-Amz-Storage-Class.
	Headers map[string]string `json:"headers"`
}

type stateObject struct {
	Key     string            `json:"key"`
	Size    int               `json:"size"`
	MD5     string            `json:"md5"`
	MTime   string            `json:"mtime"`
	Headers map[string]string `json:"headers"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:19090", "listen address")
	minPart := flag.Int64("min-part-size", 5<<20, "smallest allowed non-final multipart part")
	maxKeys := flag.Int("max-keys", 1000, "listing page size cap")
	requireMD5 := flag.Bool("require-delete-md5", false, "reject DeleteObjects without Content-MD5")
	noPartCopy := flag.Bool("disable-upload-part-copy", false, "answer UploadPartCopy with 501")
	verbose := flag.Bool("v", false, "log every request")
	flag.Parse()

	srv := s3fake.New()
	srv.MinPartSize = *minPart
	srv.MaxKeys = *maxKeys
	srv.RequireDeleteMD5 = *requireMD5
	srv.DisableUploadPartCopy = *noPartCopy

	mux := http.NewServeMux()
	mux.HandleFunc("/_gs3fake/requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			srv.ResetRequests()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(srv.Requests())
	})
	mux.HandleFunc("/_gs3fake/reset", func(w http.ResponseWriter, r *http.Request) {
		srv.Reset()
	})
	mux.HandleFunc("/_gs3fake/state", func(w http.ResponseWriter, r *http.Request) {
		state := map[string][]stateObject{}
		for _, b := range srv.Buckets() {
			objects := []stateObject{}
			for _, o := range srv.Objects(b) {
				sum := md5.Sum(o.Data)
				headers := map[string]string{}
				for name, values := range o.Header {
					headers[name] = strings.Join(values, ",")
				}
				objects = append(objects, stateObject{
					Key: o.Key, Size: len(o.Data), MD5: hex.EncodeToString(sum[:]),
					MTime: o.Modified.Format(time.RFC3339Nano), Headers: headers,
				})
			}
			state[b] = objects
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("/_gs3fake/seed", func(w http.ResponseWriter, r *http.Request) {
		var objects []seedObject
		if err := json.NewDecoder(r.Body).Decode(&objects); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, o := range objects {
			data := []byte(o.Data)
			if o.Data == "" && o.Size > 0 {
				data = patternBytes(o.Key, o.Size)
			}
			var mtime time.Time
			if o.MTime != "" {
				t, err := time.Parse(time.RFC3339Nano, o.MTime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mtime = t
			}
			header := http.Header{}
			for k, v := range o.Headers {
				header.Set(k, v)
			}
			if o.Key == "" {
				// A seed entry without a key creates the bucket; its mtime
				// is the bucket's creation date.
				srv.CreateBucket(o.Bucket, mtime)
				continue
			}
			srv.PutObject(o.Bucket, o.Key, data, mtime, header)
		}
	})
	mux.Handle("/", srv)

	var handler http.Handler = mux
	if *verbose {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("%s %s", r.Method, r.URL.RequestURI())
			mux.ServeHTTP(w, r)
		})
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "s3fake:", err)
		os.Exit(1)
	}
	fmt.Printf("s3fake listening on http://%s\n", ln.Addr())
	if err := http.Serve(ln, handler); err != nil {
		fmt.Fprintln(os.Stderr, "s3fake:", err)
		os.Exit(1)
	}
}

// patternBytes generates deterministic content that differs per key.
func patternBytes(key string, size int) []byte {
	seed := md5.Sum([]byte(key))
	out := make([]byte, size)
	for i := range out {
		out[i] = seed[i%len(seed)] + byte(i/len(seed))
	}
	return out
}
