package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gs3/internal/pyfmt"
)

// NewParamLoader returns awscli's paramfile behaviour: an option value of the
// form file://path or fileb://path is replaced by the file's contents, and
// http:// or https:// values are fetched when followURL reports true (the
// cli_follow_urlparam setting, on by default).
func NewParamLoader(followURL func() bool) ParamLoader {
	return func(value string) (string, bool, error) {
		switch {
		case strings.HasPrefix(value, "file://"):
			data, err := readParamFile(value, "file://")
			if err != nil {
				return "", false, err
			}
			if !utf8.Valid(data) {
				return "", false, fmt.Errorf(
					"Unable to load paramfile (%s), text contents could not be decoded.  "+
						"If this is a binary file, please use the fileb:// prefix instead of the file:// prefix.",
					expandParamPath(value[len("file://"):]))
			}
			return string(data), true, nil
		case strings.HasPrefix(value, "fileb://"):
			data, err := readParamFile(value, "fileb://")
			if err != nil {
				return "", false, err
			}
			return string(data), true, nil
		case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
			if followURL != nil && !followURL() {
				return "", false, nil
			}
			body, err := fetchParamURL(value)
			if err != nil {
				return "", false, fmt.Errorf("Unable to retrieve %s: %s", value, err)
			}
			return body, true, nil
		}
		return "", false, nil
	}
}

// expandParamPath is os.path.expandvars(os.path.expanduser(path)).
func expandParamPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	return os.Expand(path, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		// Python leaves unknown variables untouched.
		return "$" + name
	})
}

func readParamFile(value, prefix string) ([]byte, error) {
	path := expandParamPath(value[len(prefix):])
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Unable to load paramfile %s: %s", value, pyOSError(err, path))
	}
	return data, nil
}

// pyOSError renders a file error like Python's str(OSError).
func pyOSError(err error, path string) string {
	if msg, ok := pyfmt.OSError(err); ok {
		return msg
	}
	return err.Error()
}

func fetchParamURL(uri string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received non 200 status code of %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
