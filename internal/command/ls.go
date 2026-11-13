package command

import (
	"bufio"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gs3/internal/output"
	"gs3/internal/s3path"
)

// runLs implements `aws s3 ls`.
//
// Scripts parse this output with awk/sed and test the exit status, so the
// line formats and the "exit 1 when a key was given and nothing matched"
// rule follow awscli's ListCommand exactly.
func runLs(e *env) (int, error) {
	args := e.inv.Args
	path := strings.TrimPrefix(args.String("paths"), s3path.Scheme)
	bucket, key := s3path.FindBucketKey(path)

	client, err := e.newClient(0)
	if err != nil {
		return 0, err
	}

	out := bufio.NewWriterSize(e.io.Stdout, 64<<10)
	defer out.Flush()

	l := &lister{
		out:           out,
		humanReadable: args.Bool("human_readable"),
		atFirstPage:   true,
	}

	switch {
	case bucket == "":
		resp, err := client.ListBuckets(e.ctx, &s3.ListBucketsInput{})
		if err != nil {
			return 0, err
		}
		for _, b := range resp.Buckets {
			out.WriteString(lastModified(aws.ToTime(b.CreationDate)) + " " + aws.ToString(b.Name) + "\n")
		}
	default:
		recursive := args.Bool("dir_op")
		input := &s3.ListObjectsV2Input{
			Bucket:       aws.String(bucket),
			Prefix:       aws.String(key),
			EncodingType: types.EncodingTypeUrl,
		}
		if !recursive {
			input.Delimiter = aws.String("/")
		}
		if n, ok := args.Int("page_size"); ok {
			input.MaxKeys = aws.Int32(int32(n))
		}
		if args.IsSet("request_payer") {
			input.RequestPayer = types.RequestPayer(args.String("request_payer"))
		}
		for {
			resp, err := client.ListObjectsV2(e.ctx, input)
			if err != nil {
				out.Flush()
				return 0, err
			}
			l.displayPage(resp, !recursive)
			// Hand each page to the reader as soon as it is complete.
			out.Flush()
			if !aws.ToBool(resp.IsTruncated) || resp.NextContinuationToken == nil {
				break
			}
			input.ContinuationToken = resp.NextContinuationToken
		}
	}

	if args.Bool("summarize") {
		l.printSummary()
	}
	if key != "" && l.emptyResult && l.atFirstPage {
		// A key was given and nothing matched it.
		return exitFailure, nil
	}
	return exitOK, nil
}

type lister struct {
	out           *bufio.Writer
	humanReadable bool
	emptyResult   bool
	atFirstPage   bool
	totalSize     int64
	totalObjects  int64
}

func (l *lister) displayPage(resp *s3.ListObjectsV2Output, useBasename bool) {
	if len(resp.Contents) == 0 && len(resp.CommonPrefixes) == 0 {
		l.emptyResult = true
		return
	}
	decode := func(s *string) string { return aws.ToString(s) }
	if resp.EncodingType == types.EncodingTypeUrl {
		decode = func(s *string) string { return urlDecode(aws.ToString(s)) }
	}

	for _, p := range resp.CommonPrefixes {
		// "a/b/" -> "b": the component before the trailing delimiter.
		parts := strings.Split(decode(p.Prefix), "/")
		name := ""
		if len(parts) >= 2 {
			name = parts[len(parts)-2]
		}
		l.out.WriteString(prePrefix + name + "/\n")
	}
	for _, c := range resp.Contents {
		size := aws.ToInt64(c.Size)
		l.totalSize += size
		l.totalObjects++
		name := decode(c.Key)
		if useBasename {
			name = name[strings.LastIndexByte(name, '/')+1:]
		}
		l.out.WriteString(lastModified(aws.ToTime(c.LastModified)))
		l.out.WriteByte(' ')
		l.out.WriteString(l.sizeString(size))
		l.out.WriteByte(' ')
		l.out.WriteString(name)
		l.out.WriteByte('\n')
	}
	l.atFirstPage = false
}

// prePrefix is "PRE".rjust(30) plus the separating space.
var prePrefix = strings.Repeat(" ", 27) + "PRE "

func (l *lister) sizeString(size int64) string {
	s := strconv.FormatInt(size, 10)
	if l.humanReadable {
		s = output.HumanReadableSize(size)
	}
	if len(s) < 10 {
		s = strings.Repeat(" ", 10-len(s)) + s
	}
	return s
}

func (l *lister) printSummary() {
	size := strconv.FormatInt(l.totalSize, 10)
	if l.humanReadable {
		size = output.HumanReadableSize(l.totalSize)
	}
	l.out.WriteString("\nTotal Objects: " + strconv.FormatInt(l.totalObjects, 10) + "\n")
	l.out.WriteString("   Total Size: " + size + "\n")
}

// lastModified formats a timestamp in the local time zone, as `aws s3 ls`
// does.
func lastModified(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04:05")
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
