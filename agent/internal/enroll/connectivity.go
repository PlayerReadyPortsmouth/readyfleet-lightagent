package enroll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var ErrClockSkew = errors.New("agent/enroll: device clock differs from ReadyFleet by more than five minutes; enable automatic date and time, then retry")

// CheckConnectivity checks the pinned bootstrap TLS path (including configured proxy)
// without presenting or spending an enrollment credential. HEAD may answer
// 401/405; that still proves DNS, proxy, TLS, and endpoint connectivity.
func CheckConnectivity(ctx context.Context, endpoint string) error {
	return preflight(ctx, endpoint, newEnrollmentClient(false))
}

func preflight(ctx context.Context, endpoint string, client *http.Client) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("agent/enroll: preflight requires a configured HTTPS endpoint")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < redeemAttempts; attempt++ {
		req, err := http.NewRequestWithContext(checkCtx, http.MethodHead, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if date, dateErr := http.ParseTime(resp.Header.Get("Date")); dateErr == nil {
				skew := time.Since(date)
				if skew > 5*time.Minute || skew < -5*time.Minute {
					return ErrClockSkew
				}
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 500 && resp.StatusCode != http.StatusProxyAuthRequired && resp.StatusCode/100 != 3 {
				return nil
			}
			lastErr = fmt.Errorf("agent/enroll: preflight endpoint status %d", resp.StatusCode)
		} else {
			lastErr = fmt.Errorf("agent/enroll: preflight connection: %w", err)
		}
		if checkCtx.Err() != nil {
			return checkCtx.Err()
		}
		if attempt+1 < redeemAttempts {
			if err := retryPause(checkCtx, attempt); err != nil {
				return err
			}
		}
	}
	return lastErr
}
