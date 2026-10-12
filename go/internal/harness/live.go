package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dio/switchyard-capi/go/jev"
)

// NewLiveDecider builds a System One client from TYPESAFE_API_KEY, optionally
// overriding TYPESAFE_ENDPOINT and TYPESAFE_TIMEOUT_MS.
func NewLiveDecider() (*jev.Client, error) {
	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		return nil, errors.New("TYPESAFE_API_KEY is required for a live decider")
	}
	endpoint := os.Getenv("TYPESAFE_ENDPOINT")
	if endpoint == "" {
		endpoint = jev.DefaultEndpoint
	}
	timeout := 10 * time.Second
	if raw := os.Getenv("TYPESAFE_TIMEOUT_MS"); raw != "" {
		millis, err := strconv.Atoi(raw)
		if err != nil || millis <= 0 {
			return nil, fmt.Errorf("TYPESAFE_TIMEOUT_MS must be a positive integer, got %q", raw)
		}
		timeout = time.Duration(millis) * time.Millisecond
	}
	return jev.NewClient(endpoint, apiKey, timeout)
}

type captureFunc func(caseID string, turn int, reply Scripted) error

// captureSink returns a recorder for live replies, or nil when disabled.
func captureSink(dir string) (captureFunc, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return func(caseID string, turn int, reply Scripted) error {
		path := filepath.Join(dir, fmt.Sprintf("%s.%d.json", sanitize(caseID), turn))
		body, err := json.MarshalIndent(reply, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(body, '\n'), 0o644)
	}, nil
}

func sanitize(caseID string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(caseID)
}
