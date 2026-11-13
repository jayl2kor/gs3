// Package s3path implements the AWS CLI's rules for interpreting the paths
// given to `aws s3` commands.
package s3path

import "strings"

// Scheme is the prefix that marks an S3 path.
const Scheme = "s3://"

// IsS3 reports whether path is an S3 URI.
func IsS3(path string) bool { return strings.HasPrefix(path, Scheme) }

// FindBucketKey is awscli's find_bucket_key: it splits "bucket/key" (without
// the s3:// scheme) at the first slash.
func FindBucketKey(path string) (bucket, key string) {
	bucket, key, _ = strings.Cut(path, "/")
	return bucket, key
}

// SplitS3BucketKey is awscli's split_s3_bucket_key: FindBucketKey after
// removing an optional s3:// scheme.
func SplitS3BucketKey(path string) (bucket, key string) {
	return FindBucketKey(strings.TrimPrefix(path, Scheme))
}
