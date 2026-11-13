package command

import (
	"errors"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gs3/internal/cli"
	"gs3/internal/core"
	"gs3/internal/filter"
	"gs3/internal/listing"
	"gs3/internal/output"
	"gs3/internal/s3client"
	"gs3/internal/s3path"
	"gs3/internal/syncer"
	"gs3/internal/transfer"
	"gs3/internal/tune"
)

func init() {
	subcommands["cp"] = func(e *env) (int, error) { return runTransfer(e, "cp") }
	subcommands["mv"] = func(e *env) (int, error) { return runTransfer(e, "mv") }
	subcommands["rm"] = func(e *env) (int, error) { return runTransfer(e, "rm") }
	subcommands["sync"] = func(e *env) (int, error) { return runTransfer(e, "sync") }
}

// transferPlan is what awscli calls the command parameters: the validated,
// normalised form of a cp / mv / rm / sync invocation.
type transferPlan struct {
	cmd       string
	src, dest string // as typed, after normalisation
	pathsType string // "locals3", "s3local", "s3s3" or "s3"
	dirOp     bool
	isStream  bool
	isMove    bool
	op        core.Op
}

// allowedPathTypes lists which commands accept which combination of local
// and S3 paths.
var allowedPathTypes = map[string][]string{
	"s3s3":       {"cp", "sync", "mv"},
	"s3local":    {"cp", "sync", "mv"},
	"locals3":    {"cp", "sync", "mv"},
	"s3":         {"mb", "rb", "rm"},
	"local":      {},
	"locallocal": {},
}

var operations = map[string]core.Op{
	"locals3": core.OpUpload,
	"s3s3":    core.OpCopy,
	"s3local": core.OpDownload,
	"s3":      core.OpDelete,
}

// newTransferPlan performs the validation of awscli's CommandParameters, in
// the same order and with the same messages. Every error here is reported
// with exit status 255.
func newTransferPlan(cmd string, args cli.Namespace) (*transferPlan, error) {
	p := &transferPlan{cmd: cmd, isMove: cmd == "mv"}
	p.dirOp = args.Bool("dir_op") || cmd == "sync"

	paths := append([]string{}, args.Strings("paths")...)
	for _, path := range paths {
		if s3path.IsS3(path) {
			p.pathsType += "s3"
		} else {
			p.pathsType += "local"
		}
	}
	allowed := false
	for _, c := range allowedPathTypes[p.pathsType] {
		allowed = allowed || c == cmd
	}
	if !allowed {
		return nil, cli.Errorf("usage: aws s3 %s %s\nError: Invalid argument type", cmd, cli.SubcommandUsage(cmd))
	}
	p.op = operations[p.pathsType]

	// "s3://bucket" means the bucket root.
	for i, path := range paths {
		if s3path.IsS3(path) {
			if _, key := s3path.SplitS3BucketKey(path); key == "" && !strings.HasSuffix(path, "/") {
				paths[i] = path + "/"
			}
		}
	}
	p.src, p.dest = paths[0], paths[0]
	if len(paths) == 2 {
		p.dest = paths[1]
	}

	if p.src == "-" || p.dest == "-" {
		if cmd != "cp" || p.dirOp {
			return nil, cli.Errorf("Streaming currently is only compatible with non-recursive cp commands")
		}
		p.isStream = true
	}

	if cmd == "mv" && p.samePath() {
		return nil, cli.Errorf("Cannot mv a file onto itself: '%s' - '%s'", p.src, p.dest)
	}
	switch {
	case p.pathsType == "locals3" && !p.isStream:
		if _, err := os.Stat(p.src); err != nil {
			return nil, cli.Errorf("The user-provided path %s does not exist.", p.src)
		}
	case p.pathsType == "s3local" && p.dirOp:
		// Created up front (even for --dryrun) so the destination listing
		// does not warn about a missing directory.
		if _, err := os.Stat(p.dest); err != nil {
			if err := os.MkdirAll(p.dest, 0o777); err != nil {
				return nil, err
			}
		}
	}

	for _, kind := range []string{"sse_c", "sse_c_copy_source"} {
		flag := "--" + strings.ReplaceAll(kind, "_", "-")
		hasAlgorithm, hasKey := args.String(kind) != "", args.String(kind+"_key") != ""
		// "It" is the AWS CLI's own typo; scripts may match on the text.
		if hasAlgorithm && !hasKey {
			return nil, cli.Errorf("It %s is specified, %s-key must be specified as well.", flag, flag)
		}
		if hasKey && !hasAlgorithm {
			return nil, cli.Errorf("It %s-key is specified, %s must be specified as well.", flag, flag)
		}
	}
	if args.String("sse_c_copy_source") != "" && p.pathsType != "s3s3" {
		return nil, cli.Errorf("--sse-c-copy-source is only supported for copy operations.")
	}
	return p, nil
}

// samePath reports whether an S3-to-S3 move would land on its own source.
func (p *transferPlan) samePath() bool {
	if p.pathsType != "s3s3" {
		return false
	}
	if p.src == p.dest {
		return true
	}
	if strings.HasSuffix(p.dest, "/") {
		base := p.src[strings.LastIndex(p.src, "/")+1:]
		return p.src == p.dest+base
	}
	return false
}

// requestParams maps flags to request parameters. The returned error, if any,
// is one the AWS CLI raises when the first upload or copy is submitted.
func requestParams(args cli.Namespace) (transfer.Params, error) {
	p := transfer.Params{
		ACL:                     args.String("acl"),
		StorageClass:            args.String("storage_class"),
		WebsiteRedirect:         args.String("website_redirect"),
		ContentType:             args.String("content_type"),
		CacheControl:            args.String("cache_control"),
		ContentDisposition:      args.String("content_disposition"),
		ContentEncoding:         args.String("content_encoding"),
		ContentLanguage:         args.String("content_language"),
		Metadata:                args.Map("metadata"),
		MetadataDirective:       args.String("metadata_directive"),
		SSE:                     args.String("sse"),
		SSEKMSKeyID:             args.String("sse_kms_key_id"),
		SSECAlgorithm:           args.String("sse_c"),
		SSECKey:                 args.String("sse_c_key"),
		CopySourceSSECAlgorithm: args.String("sse_c_copy_source"),
		CopySourceSSECKey:       args.String("sse_c_copy_source_key"),
		RequestPayer:            args.String("request_payer"),
	}
	var firstErr error
	if err := p.SetGrants(args.Strings("grants")); err != nil {
		firstErr = err
	}
	if v := args.String("expires"); v != "" {
		t, err := transfer.ParseExpires(v)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		p.Expires = &t
	}
	return p, firstErr
}

func printerMode(args cli.Namespace, isStream bool) output.Mode {
	switch {
	case args.Bool("quiet"):
		return output.ModeQuiet
	case args.Bool("only_show_errors"), isStream:
		return output.ModeOnlyErrors
	case args["progress"] == false:
		return output.ModeNoProgress
	}
	return output.ModeFull
}

// runTransfer implements cp, mv, rm and sync.
func runTransfer(e *env, cmd string) (int, error) {
	args := e.inv.Args
	plan, err := newTransferPlan(cmd, args)
	if err != nil {
		return 0, err
	}

	limits := tune.Detect()
	tune.ApplyMemoryLimit(limits)
	settings := tune.Resolve(limits, e.settings.S3, e.getenv)

	limiter := transfer.NewLimiter(settings.Concurrency)
	throttle := transfer.NewAdaptive(limiter, settings.Concurrency)
	if isOff(e.getenv("GS3_ADAPTIVE")) {
		throttle = nil
	}

	clientCfg := e.clientConfig(settings.Concurrency + 16)
	if throttle != nil {
		clientCfg.OnThrottle = throttle.Throttled
	}
	client, err := s3client.New(e.ctx, clientCfg)
	if err != nil {
		return 0, err
	}
	sourceClient := client
	if region := args.String("source_region"); region != "" && plan.pathsType == "s3s3" {
		// The source client ignores --endpoint-url, as in the AWS CLI; an
		// endpoint configured by environment or config file still applies.
		srcCfg := clientCfg
		srcCfg.Region = region
		srcSettings := *e.settings
		if e.inv.Globals.EndpointURL != "" {
			srcSettings.EndpointURL = e.settings.ConfiguredEndpointURL
		}
		srcCfg.Settings = &srcSettings
		if sourceClient, err = s3client.New(e.ctx, srcCfg); err != nil {
			return 0, err
		}
	}

	params, paramErr := requestParams(args)
	cwd, _ := os.Getwd()

	reporter := output.NewReporter(e.io.Stdout, e.io.Stderr, printerMode(args, plan.isStream))
	engine := transfer.New(e.ctx, transfer.Config{
		Client:                client,
		SourceClient:          sourceClient,
		Reporter:              reporter,
		Limiter:               limiter,
		QueueSize:             settings.QueueSize,
		MultipartThreshold:    settings.MultipartThreshold,
		ChunkSize:             settings.ChunkSize,
		CopyChunkSize:         settings.CopyChunkSize,
		MaxParts:              settings.MaxParts,
		StreamChunkSize:       settings.StreamChunkSize,
		StreamWindow:          settings.StreamWindow,
		ContentMD5:            settings.ContentMD5,
		BatchDelete:           settings.BatchDelete && plan.dirOp,
		MaxBandwidth:          settings.MaxBandwidth,
		Params:                params,
		IsMove:                plan.isMove,
		DryRun:                args.Bool("dryrun"),
		GuessMIME:             args["guess_mime_type"] != false,
		ForceGlacierTransfer:  args.Bool("force_glacier_transfer"),
		IgnoreGlacierWarnings: args.Bool("ignore_glacier_warnings"),
		Cwd:                   cwd,
		Stdin:                 e.io.Stdin,
		Stdout:                e.io.Stdout,
	})
	if throttle != nil {
		stop := throttle.Run()
		defer stop()
	}

	// fatal reports an error that ends the command ("fatal error: ...") and
	// stops everything in flight.
	fatal := func(err error) {
		if err == nil || errors.Is(err, e.ctx.Err()) && e.ctx.Err() != nil {
			return
		}
		reporter.Send(output.Result{Kind: output.Fatal, Message: output.ErrorMessage(err)})
		engine.Abort(err)
	}
	// submit hands one file to the engine. Invalid --grants / --expires
	// values surface here, when the first upload or copy is about to start.
	submit := func(fs core.FileStat) bool {
		if paramErr != nil && (fs.Op == core.OpUpload || fs.Op == core.OpCopy) {
			fatal(paramErr)
			return false
		}
		return engine.Submit(fs)
	}

	if plan.isStream {
		files := s3path.Format(plan.src, plan.dest, false)
		if paramErr != nil && plan.op == core.OpUpload {
			fatal(paramErr)
		} else {
			engine.Stream(files, plan.op, args.String("expected_size"))
		}
	} else {
		runPipeline(e, plan, args, client, sourceClient, &params, reporter, submit, fatal)
	}

	engine.Close()
	if e.ctx.Err() != nil {
		reporter.Send(output.Result{Kind: output.Cancelled})
	}
	return reporter.Close().ExitCode(), nil
}

// runPipeline lists, filters, compares (for sync) and submits.
func runPipeline(
	e *env, plan *transferPlan, args cli.Namespace,
	client, sourceClient *s3client.Client, params *transfer.Params,
	reporter *output.Reporter, submit func(core.FileStat) bool, fatal func(error),
) {
	followSymlinks := args["follow_symlinks"] != false
	pageSize, _ := args.Int("page_size")

	rules := make([]filter.Rule, 0, len(args.Filters()))
	for _, r := range args.Filters() {
		rules = append(rules, filter.Rule{Include: r.Option == "--include", Pattern: r.Pattern})
	}
	flt := filter.New(rules, plan.src, plan.dest, plan.dirOp)

	// list starts the listing of one side. The source of a copy is read with
	// the source client and the copy-source SSE-C key.
	list := func(files s3path.Files, op core.Op, c *s3client.Client, isSource bool) *listing.Stream {
		if files.Src.Type == core.Local {
			return listing.Local(e.ctx, files, op, followSymlinks, reporter)
		}
		return listing.S3(e.ctx, c, files, op, listing.S3Options{
			PageSize:     pageSize,
			RequestPayer: params.RequestPayer,
			Head: func(in *s3.HeadObjectInput) {
				if isSource {
					params.ApplyHead(in, plan.op == core.OpCopy)
				}
			},
		})
	}

	files := s3path.Format(plan.src, plan.dest, plan.dirOp)
	src := filtered(e, list(files, plan.op, sourceClient, true), flt)

	if plan.cmd != "sync" {
		for batch := range src.C {
			for i := range batch {
				if !submit(batch[i]) {
					break
				}
			}
		}
		fatal(src.Err())
		return
	}

	reverse := s3path.Format(plan.dest, plan.src, plan.dirOp)
	dst := filtered(e, list(reverse, core.OpNone, client, false), flt)
	err := syncer.Run(e.ctx, src, dst, syncer.Options{
		SizeOnly:        args.Bool("size_only"),
		ExactTimestamps: args.Bool("exact_timestamps"),
		Delete:          args.Bool("delete"),
	}, submit)
	fatal(err)
}

// filtered applies the include/exclude rules to a listing.
func filtered(e *env, in *listing.Stream, flt *filter.Filter) *listing.Stream {
	if flt.Empty() {
		return in
	}
	return listing.Filter(e.ctx, in, flt.Match)
}

func isOff(v string) bool {
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}
