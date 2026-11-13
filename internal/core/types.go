// Package core holds the types shared by the listing, sync and transfer
// stages of the pipeline.
package core

import "time"

// PathType says where a path lives.
type PathType uint8

const (
	Local PathType = iota
	S3
)

func (t PathType) String() string {
	if t == S3 {
		return "s3"
	}
	return "local"
}

// Op is what is done with a file; awscli calls it the operation name.
type Op uint8

const (
	// OpNone marks entries from the destination side of a sync, which only
	// exist to be compared against.
	OpNone Op = iota
	OpUpload
	OpDownload
	OpCopy
	OpDelete
)

func (o Op) String() string {
	switch o {
	case OpUpload:
		return "upload"
	case OpDownload:
		return "download"
	case OpCopy:
		return "copy"
	case OpDelete:
		return "delete"
	}
	return ""
}

// FileStat is one local file or S3 object produced by a lister, together with
// where it would go. It is awscli's FileStat/FileInfo.
type FileStat struct {
	// Src is the full source path: an absolute local path, or "bucket/key".
	Src string
	// Dest is the full destination path in the same form.
	Dest string
	// CompareKey is the path relative to the listing root, with "/"
	// separators; sync pairs source and destination entries by it.
	CompareKey string

	Size int64
	// SizeKnown is false only for a single-object delete, where no
	// HeadObject is issued.
	SizeKnown bool
	// ModTime is the last modification time; zero when unknown.
	ModTime time.Time

	SrcType  PathType
	DestType PathType
	Op       Op

	// StorageClass and Restore come from ListObjectsV2 / HeadObject and
	// drive the Glacier checks. Restore is only ever set by HeadObject.
	StorageClass string
	Restore      string
	ETag         string
}

// IsGlacier reports whether the entry is an archived object that has not been
// restored, i.e. one that cannot be downloaded or copied.
func (f *FileStat) IsGlacier() bool {
	if f.StorageClass != "GLACIER" && f.StorageClass != "DEEP_ARCHIVE" {
		return false
	}
	return !containsRestored(f.Restore)
}

func containsRestored(restore string) bool {
	const marker = `ongoing-request="false"`
	for i := 0; i+len(marker) <= len(restore); i++ {
		if restore[i:i+len(marker)] == marker {
			return true
		}
	}
	return false
}
