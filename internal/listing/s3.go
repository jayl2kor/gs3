package listing

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gs3/internal/core"
	"gs3/internal/s3path"
)

// API is the part of the S3 client a listing needs.
type API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// S3Options configure an S3 listing.
type S3Options struct {
	// PageSize is --page-size; 0 lets the service decide (1000).
	PageSize     int
	RequestPayer string
	// Head, when set, adds SSE-C parameters to the HeadObject of a single
	// object.
	Head func(*s3.HeadObjectInput)
}

// KeyNotFoundError is a HeadObject 404 on the single object a command names.
type KeyNotFoundError struct{ Key string }

func (e *KeyNotFoundError) Error() string {
	return `An error occurred (404) when calling the HeadObject operation: Key "` + e.Key + `" does not exist`
}

// S3 lists the objects of an S3 source, as awscli's
// FileGenerator.list_objects does.
func S3(ctx context.Context, client API, files s3path.Files, op core.Op, opts S3Options) *Stream {
	return start(ctx, 2, func(emit func([]core.FileStat) bool) error {
		bucket, prefix := s3path.FindBucketKey(files.Src.Path)

		// A single named object needs no listing: one HeadObject (or, for a
		// delete, nothing at all).
		if !files.DirOp && prefix != "" {
			fs, err := single(ctx, client, files, op, opts, bucket, prefix)
			if err != nil {
				return err
			}
			emit([]core.FileStat{fs})
			return nil
		}

		input := &s3.ListObjectsV2Input{
			Bucket:       aws.String(bucket),
			Prefix:       aws.String(prefix),
			EncodingType: types.EncodingTypeUrl,
			RequestPayer: types.RequestPayer(opts.RequestPayer),
		}
		if opts.PageSize > 0 {
			input.MaxKeys = aws.Int32(int32(opts.PageSize))
		}
		if !files.DirOp {
			// `cp s3://bucket/ dst` without --recursive transfers nothing,
			// but the AWS CLI still lists (and so still reports a missing
			// bucket). One key is enough to get the same errors.
			input.MaxKeys = aws.Int32(1)
			_, err := client.ListObjectsV2(ctx, input)
			return err
		}

		for {
			page, err := client.ListObjectsV2(ctx, input)
			if err != nil {
				return err
			}
			encoded := page.EncodingType == types.EncodingTypeUrl
			batch := make([]core.FileStat, 0, len(page.Contents))
			for i := range page.Contents {
				c := &page.Contents[i]
				key := aws.ToString(c.Key)
				if encoded {
					key = urlDecode(key)
				}
				size := aws.ToInt64(c.Size)
				// Zero-byte keys ending in "/" are folder markers created by
				// consoles. They are not downloaded or copied, but a delete
				// must remove them.
				if size == 0 && strings.HasSuffix(key, "/") && op != core.OpDelete {
					continue
				}
				src := bucket + "/" + key
				dest, compareKey := files.DestAndCompareKey(src)
				batch = append(batch, core.FileStat{
					Src: src, Dest: dest, CompareKey: compareKey,
					Size: size, SizeKnown: true,
					ModTime: aws.ToTime(c.LastModified),
					SrcType: core.S3, DestType: files.Dest.Type, Op: op,
					StorageClass: string(c.StorageClass),
					ETag:         aws.ToString(c.ETag),
				})
			}
			if !emit(batch) {
				return nil
			}
			if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
				return nil
			}
			input.ContinuationToken = page.NextContinuationToken
		}
	})
}

func single(ctx context.Context, client API, files s3path.Files, op core.Op, opts S3Options, bucket, key string) (core.FileStat, error) {
	src := files.Src.Path
	dest, compareKey := files.DestAndCompareKey(src)
	fs := core.FileStat{
		Src: src, Dest: dest, CompareKey: compareKey,
		SrcType: core.S3, DestType: files.Dest.Type, Op: op,
	}
	if op == core.OpDelete {
		// Nothing HeadObject returns is needed to delete an object.
		return fs, nil
	}
	input := &s3.HeadObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		RequestPayer: types.RequestPayer(opts.RequestPayer),
	}
	if opts.Head != nil {
		opts.Head(input)
	}
	head, err := client.HeadObject(ctx, input)
	if err != nil {
		if isNotFound(err) {
			return fs, &KeyNotFoundError{Key: key}
		}
		return fs, err
	}
	fs.Size, fs.SizeKnown = aws.ToInt64(head.ContentLength), true
	fs.ModTime = aws.ToTime(head.LastModified)
	fs.StorageClass = string(head.StorageClass)
	fs.Restore = aws.ToString(head.Restore)
	fs.ETag = aws.ToString(head.ETag)
	return fs, nil
}

func isNotFound(err error) bool {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotFound"
}

// urlDecode reverses S3's encoding-type=url (spaces arrive as "+").
func urlDecode(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}
