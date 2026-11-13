package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gs3/internal/cli"
	"gs3/internal/output"
	"gs3/internal/s3path"
)

// runMb implements `aws s3 mb`.
func runMb(e *env) (int, error) {
	path := e.inv.Args.String("path")
	if !s3path.IsS3(path) {
		return 0, cli.Errorf("<S3Uri>\nError: Invalid argument type")
	}
	bucket, _ := s3path.SplitS3BucketKey(path)

	client, err := e.newClient(0)
	if err != nil {
		return 0, err
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if client.Region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(client.Region),
		}
	}
	if _, err := client.CreateBucket(e.ctx, input); err != nil {
		fmt.Fprintf(e.io.Stderr, "make_bucket failed: %s %s\n", path, output.ErrorMessage(err))
		return exitFailure, nil
	}
	fmt.Fprintf(e.io.Stdout, "make_bucket: %s\n", bucket)
	return exitOK, nil
}

// runRb implements `aws s3 rb`.
func runRb(e *env) (int, error) {
	path := e.inv.Args.String("path")
	if !s3path.IsS3(path) {
		return 0, cli.Errorf("<S3Uri>\nError: Invalid argument type")
	}
	bucket, key := s3path.SplitS3BucketKey(path)
	if key != "" {
		return 0, cli.Errorf("Please specify a valid bucket name only. E.g. s3://%s", bucket)
	}

	if e.inv.Args.Bool("force") {
		// `rb --force` is `rm --recursive` on the bucket, with the same
		// global options, followed by the bucket removal.
		rm, err := cli.Parse([]string{"s3", "rm", path, "--recursive"}, nil)
		if err != nil {
			return 0, err
		}
		rm.Globals = e.inv.Globals
		sub := *e
		sub.inv = rm
		rc, err := runTransfer(&sub, "rm")
		if err != nil {
			return 0, err
		}
		if rc != 0 {
			return 0, cli.Errorf("remove_bucket failed: Unable to delete all objects in the bucket, bucket will not be deleted.")
		}
	}

	client, err := e.newClient(0)
	if err != nil {
		return 0, err
	}
	if _, err := client.DeleteBucket(e.ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		fmt.Fprintf(e.io.Stderr, "remove_bucket failed: %s %s\n", path, output.ErrorMessage(err))
		return exitFailure, nil
	}
	fmt.Fprintf(e.io.Stdout, "remove_bucket: %s\n", bucket)
	return exitOK, nil
}

// runWebsite implements `aws s3 website`.
func runWebsite(e *env) (int, error) {
	args := e.inv.Args
	// Accepts "s3://bucket" or "bucket", with an optional trailing slash.
	bucket := strings.TrimPrefix(args.Strings("paths")[0], s3path.Scheme)
	bucket = strings.TrimSuffix(bucket, "/")

	cfg := &types.WebsiteConfiguration{}
	if args.IsSet("index_document") {
		cfg.IndexDocument = &types.IndexDocument{Suffix: aws.String(args.String("index_document"))}
	}
	if args.IsSet("error_document") {
		cfg.ErrorDocument = &types.ErrorDocument{Key: aws.String(args.String("error_document"))}
	}

	client, err := e.newClient(0)
	if err != nil {
		return 0, err
	}
	_, err = client.PutBucketWebsite(e.ctx, &s3.PutBucketWebsiteInput{
		Bucket:               aws.String(bucket),
		WebsiteConfiguration: cfg,
	})
	return exitOK, err
}

// runPresign implements `aws s3 presign`.
func runPresign(e *env) (int, error) {
	args := e.inv.Args
	bucket, key := s3path.SplitS3BucketKey(args.String("path"))
	expires, _ := args.Int("expires_in")

	client, err := e.newClient(0)
	if err != nil {
		return 0, err
	}
	req, err := s3.NewPresignClient(client.Client).PresignGetObject(e.ctx,
		&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)},
		s3.WithPresignExpires(time.Duration(expires)*time.Second))
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(e.io.Stdout, req.URL)
	return exitOK, nil
}
