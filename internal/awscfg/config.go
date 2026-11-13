package awscfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Overrides are values given on the command line; they win over everything.
type Overrides struct {
	Profile     string
	Region      string
	EndpointURL string
	CABundle    string
}

// Settings is the configuration resolved from the command line, environment
// and shared config file.
type Settings struct {
	ConfigFile      string
	CredentialsFile string

	// Profile is the selected profile name; ProfileExplicit reports whether
	// the user chose it (flag or environment) rather than falling back to
	// "default".
	Profile         string
	ProfileExplicit bool

	// Region is "" when nothing configures one.
	Region string
	// EndpointURL is "" when the default AWS endpoint applies.
	EndpointURL string
	// ConfiguredEndpointURL is the endpoint from the environment or config
	// file alone, ignoring --endpoint-url.
	ConfiguredEndpointURL string
	CABundle              string
	// MaxAttempts is 0 when not configured.
	MaxAttempts int
	// FollowURLParam is the cli_follow_urlparam setting (default true).
	FollowURLParam bool

	S3 S3Settings
}

// S3Settings holds the nested `s3 =` block. Zero values mean "not set", so
// callers can tell an explicit setting from a default.
type S3Settings struct {
	MaxConcurrentRequests int
	MaxQueueSize          int
	MultipartThreshold    int64
	MultipartChunksize    int64
	// MaxBandwidth is in bytes per second.
	MaxBandwidth int64

	// AddressingStyle is "", "auto", "path" or "virtual".
	AddressingStyle string
	// PayloadSigningEnabled is nil when not set.
	PayloadSigningEnabled *bool
	UseAccelerateEndpoint bool
	UseDualstackEndpoint  bool
}

// ConfigError is a configuration problem the AWS CLI reports with exit
// status 255.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func configErrorf(format string, args ...any) error {
	return &ConfigError{Msg: fmt.Sprintf(format, args...)}
}

// Load resolves settings in botocore's order of precedence: command line,
// then environment, then the profile in the shared config file.
//
// Notable differences from the Go SDK's own loader, kept on purpose because
// they are what AWS CLI 1.31.13 does:
//   - the region comes from AWS_DEFAULT_REGION only; AWS_REGION is ignored;
//   - AWS_DEFAULT_PROFILE is consulted before AWS_PROFILE.
func Load(getenv func(string) string, o Overrides) (*Settings, error) {
	s := &Settings{FollowURLParam: true}

	s.ConfigFile = expandPath(firstNonEmpty(getenv("AWS_CONFIG_FILE"), "~/.aws/config"))
	s.CredentialsFile = expandPath(firstNonEmpty(getenv("AWS_SHARED_CREDENTIALS_FILE"), "~/.aws/credentials"))

	s.Profile = firstNonEmpty(o.Profile, getenv("AWS_DEFAULT_PROFILE"), getenv("AWS_PROFILE"))
	s.ProfileExplicit = s.Profile != ""
	if !s.ProfileExplicit {
		s.Profile = "default"
	}

	config, err := parseINI(s.ConfigFile)
	if err != nil {
		return nil, configErrorf("Unable to parse config file: %s", s.ConfigFile)
	}
	scoped, found := profileSection(config, s.Profile)
	if !found && s.ProfileExplicit {
		// A profile may also exist only in the credentials file.
		creds, err := parseINI(s.CredentialsFile)
		if err != nil {
			return nil, configErrorf("Unable to parse config file: %s", s.CredentialsFile)
		}
		if _, ok := creds[s.Profile]; !ok {
			return nil, configErrorf("The config profile (%s) could not be found", s.Profile)
		}
	}

	s.Region = firstNonEmpty(o.Region, getenv("AWS_DEFAULT_REGION"), scoped["region"])
	s.CABundle = firstNonEmpty(o.CABundle, getenv("AWS_CA_BUNDLE"), scoped["ca_bundle"])
	s.FollowURLParam = firstNonEmpty(scoped["cli_follow_urlparam"], "true") == "true"

	if v := firstNonEmpty(getenv("AWS_MAX_ATTEMPTS"), scoped["max_attempts"]); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, configErrorf("invalid literal for int() with base 10: '%s'", v)
		}
		s.MaxAttempts = n
	}

	ignoreConfigured := isTrue(firstNonEmpty(getenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"), scoped["ignore_configured_endpoint_urls"]))
	if !ignoreConfigured {
		s.ConfiguredEndpointURL = firstNonEmpty(
			getenv("AWS_ENDPOINT_URL_S3"),
			getenv("AWS_ENDPOINT_URL"),
			servicesEndpoint(config, scoped["services"]),
			scoped["endpoint_url"],
		)
	}
	s.EndpointURL = firstNonEmpty(o.EndpointURL, s.ConfiguredEndpointURL)

	if err := s.S3.load(scoped["s3"]); err != nil {
		return nil, err
	}
	if err := s.S3.applyEnv(getenv); err != nil {
		return nil, err
	}
	return s, nil
}

// profileSection finds a profile's options. The default profile may be
// written as [default] or [profile default].
func profileSection(config iniFile, name string) (map[string]string, bool) {
	merged := map[string]string{}
	found := false
	for header, opts := range config {
		match := false
		if header == "default" {
			match = name == "default"
		} else if n, ok := sectionName(header, "profile"); ok {
			match = n == name
		}
		if match {
			found = true
			for k, v := range opts {
				merged[k] = v
			}
		}
	}
	return merged, found
}

// servicesEndpoint reads `s3 = endpoint_url = ...` from the [services NAME]
// section a profile points at.
func servicesEndpoint(config iniFile, name string) string {
	if name == "" {
		return ""
	}
	for header, opts := range config {
		if n, ok := sectionName(header, "services"); ok && n == name {
			if block := opts["s3"]; strings.HasPrefix(block, "\n") {
				if nested, err := parseNested(block); err == nil {
					return nested["endpoint_url"]
				}
			}
		}
	}
	return ""
}

// load converts the nested `s3 =` block, mirroring
// awscli.customizations.s3.transferconfig.RuntimeConfig.
func (c *S3Settings) load(block string) error {
	if !strings.HasPrefix(block, "\n") {
		return nil
	}
	nested, err := parseNested(block)
	if err != nil {
		return configErrorf("Unable to parse config file: %s", err)
	}
	return c.apply(nested)
}

func (c *S3Settings) apply(values map[string]string) error {
	for _, name := range []string{"multipart_chunksize", "multipart_threshold"} {
		raw, ok := values[name]
		if !ok {
			continue
		}
		n, err := ParseSize(raw)
		if err != nil {
			return err
		}
		if n <= 0 {
			return positiveError(name, raw)
		}
		if name == "multipart_chunksize" {
			c.MultipartChunksize = n
		} else {
			c.MultipartThreshold = n
		}
	}
	if raw, ok := values["max_bandwidth"]; ok {
		if !strings.HasSuffix(raw, "B/s") {
			return configErrorf("Invalid rate: %s. The value must be expressed as a rate in terms of bytes per seconds (e.g. 10MB/s or 800KB/s)", raw)
		}
		n, err := ParseSize(strings.TrimSuffix(raw, "/s"))
		if err != nil {
			return err
		}
		if n <= 0 {
			return positiveError("max_bandwidth", raw)
		}
		c.MaxBandwidth = n
	}
	for _, name := range []string{"max_concurrent_requests", "max_queue_size"} {
		raw, ok := values[name]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 {
			return positiveError(name, raw)
		}
		if name == "max_concurrent_requests" {
			c.MaxConcurrentRequests = n
		} else {
			c.MaxQueueSize = n
		}
	}

	if v, ok := values["addressing_style"]; ok {
		c.AddressingStyle = v
	}
	if v, ok := values["payload_signing_enabled"]; ok {
		b := isTrue(v)
		c.PayloadSigningEnabled = &b
	}
	c.UseAccelerateEndpoint = isTrue(values["use_accelerate_endpoint"])
	c.UseDualstackEndpoint = isTrue(values["use_dualstack_endpoint"])
	return nil
}

// applyEnv lets GS3_* variables override the transfer settings. These are
// gs3 extensions; the flag surface stays identical to the AWS CLI.
func (c *S3Settings) applyEnv(getenv func(string) string) error {
	values := map[string]string{}
	for env, key := range map[string]string{
		"GS3_MAX_CONCURRENT_REQUESTS": "max_concurrent_requests",
		"GS3_MAX_QUEUE_SIZE":          "max_queue_size",
		"GS3_MULTIPART_THRESHOLD":     "multipart_threshold",
		"GS3_MULTIPART_CHUNKSIZE":     "multipart_chunksize",
		"GS3_MAX_BANDWIDTH":           "max_bandwidth",
	} {
		if v := getenv(env); v != "" {
			values[key] = v
		}
	}
	if len(values) == 0 {
		return nil
	}
	// Keep the options that only the config file can set.
	saved := *c
	if err := c.apply(values); err != nil {
		return err
	}
	c.AddressingStyle = saved.AddressingStyle
	c.PayloadSigningEnabled = saved.PayloadSigningEnabled
	c.UseAccelerateEndpoint = saved.UseAccelerateEndpoint
	c.UseDualstackEndpoint = saved.UseDualstackEndpoint
	return nil
}

func positiveError(name, value string) error {
	return configErrorf("Value for %s must be a positive integer: %s", name, value)
}

var sizeSuffixes = map[string]int64{
	"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// ParseSize is awscli's human_readable_to_bytes: an integer, optionally
// followed by KB/MB/GB/TB or KiB/MiB/GiB/TiB. Every suffix is a power of 1024.
func ParseSize(value string) (int64, error) {
	v := strings.ToLower(value)
	suffix := ""
	switch {
	case len(v) >= 3 && strings.HasSuffix(v, "ib"):
		suffix = v[len(v)-3:]
	case len(v) >= 2:
		suffix = v[len(v)-2:]
	}
	mult, ok := sizeSuffixes[suffix]
	if !ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, configErrorf("Invalid size value: %s", v)
		}
		return n, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[:len(v)-len(suffix)]), 10, 64)
	if err != nil {
		return 0, configErrorf("invalid literal for int() with base 10: '%s'", v[:len(v)-len(suffix)])
	}
	return n * mult, nil
}

func isTrue(v string) bool { return strings.ToLower(v) == "true" }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// expandPath is os.path.expanduser(os.path.expandvars(path)).
func expandPath(path string) string {
	path = os.Expand(path, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return "$" + name
	})
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return path
}
