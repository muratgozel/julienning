package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Healthz checks that GET <base>/healthz answers 200. No token is sent.
func Healthz(ctx context.Context, base string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("cannot reach the Worker at %s: %v", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for keep-alive; content is irrelevant
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the Worker at %s answered %d on /healthz (is the URL the Worker's base URL?)", base, resp.StatusCode)
	}
	return nil
}

// IsAuth reports a Worker 401/403: the team token is wrong.
func IsAuth(err error) bool {
	var re *Error
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}
