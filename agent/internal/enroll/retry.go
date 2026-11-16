package enroll

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// ErrAmbiguousRedemption means a POST may already have consumed the secret.
// Retrying it would hide the real failure behind "already used". A fresh link
// is required unless a saved pending receipt exists.
var ErrAmbiguousRedemption = errors.New("agent/enroll: enrollment response was lost; request a fresh setup link")

const checkpointName = ".enrollment-recovery.json"
const redeemAttempts = 3

// Contains private key and pending poll credential. This is never logged and
// uses the same restricted writer as the final credential material. It is
// bound to both endpoint and bootstrap secret, so another setup link cannot
// accidentally resume a different device's enrollment.
type enrollmentCheckpoint struct {
	Binding   string         `json:"binding"`
	KeyPEM    string         `json:"key_pem"`
	Attempted bool           `json:"attempted"`
	Pending   *pendingRedeem `json:"pending,omitempty"`
	Response  *Response      `json:"response,omitempty"`
}

func checkpointBinding(opts Options) string {
	h := sha256.Sum256([]byte(opts.EnrollmentURL + "\x00" + opts.BearerSecret))
	return hex.EncodeToString(h[:])
}

func saveCheckpoint(opts Options, state enrollmentCheckpoint) error {
	if !opts.Retry {
		return nil
	}
	if err := os.MkdirAll(opts.MaterialDir, 0o700); err != nil {
		return fmt.Errorf("agent/enroll: prepare recovery directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := writeCheckpoint(filepath.Join(opts.MaterialDir, checkpointName), data); err != nil {
		return fmt.Errorf("agent/enroll: save recovery checkpoint: %w", err)
	}
	return nil
}

func newEnrollmentClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: bootstrapTLSConfig(insecure)},
		// Redirects are not part of the enrollment protocol. Even same-host
		// redirects could disclose bootstrap or poll credentials to another path.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func exchange(ctx context.Context, opts Options) (*rsa.PrivateKey, Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, Response{}, err
	}
	state := enrollmentCheckpoint{Binding: checkpointBinding(opts)}
	if opts.Retry {
		f, err := os.Open(filepath.Join(opts.MaterialDir, checkpointName))
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(f, 64*1024+1))
			_ = f.Close()
			if readErr != nil || len(data) > 64*1024 || json.Unmarshal(data, &state) != nil || state.Binding != checkpointBinding(opts) {
				return nil, Response{}, errors.New("agent/enroll: recovery checkpoint does not match this setup link; request a fresh setup link")
			}
		} else if !os.IsNotExist(err) {
			return nil, Response{}, fmt.Errorf("agent/enroll: read recovery checkpoint: %w", err)
		}
	}
	var key *rsa.PrivateKey
	var err error
	if state.KeyPEM != "" {
		block, _ := pem.Decode([]byte(state.KeyPEM))
		if block == nil {
			return nil, Response{}, errors.New("agent/enroll: invalid recovery key")
		}
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else {
		key, err = GenerateKey()
	}
	if err != nil {
		return nil, Response{}, fmt.Errorf("agent/enroll: keygen: %w", err)
	}
	state.KeyPEM = string(EncodeKeyPEM(key))
	client := opts.HTTPClient
	if client == nil {
		client = newEnrollmentClient(opts.Insecure)
	} else {
		// Preserve the injected transport and timeout while enforcing the same
		// no-redirect security rule as the production client.
		copyClient := *client
		copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copyClient
	}
	if state.Response != nil {
		return key, *state.Response, nil
	}
	if state.Pending != nil {
		approved, err := waitForApproval(ctx, client, opts, *state.Pending)
		if err != nil {
			return nil, Response{}, err
		}
		state.Response = approved
		if err := saveCheckpoint(opts, state); err != nil {
			return nil, Response{}, err
		}
		return key, *approved, nil
	}
	if state.Attempted {
		return nil, Response{}, ErrAmbiguousRedemption
	}
	csr, err := GenerateCSR(key)
	if err != nil {
		return nil, Response{}, err
	}
	body, err := json.Marshal(Request{CSRPEM: string(csr), AgentVersion: opts.AgentVersion, Inventory: opts.Inventory})
	if err != nil {
		return nil, Response{}, fmt.Errorf("agent/enroll: marshal: %w", err)
	}
	attempts := 1
	if opts.Retry {
		attempts = redeemAttempts
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, Response{}, err
		}
		state.Attempted = true
		if err := saveCheckpoint(opts, state); err != nil {
			return nil, Response{}, err
		}
		var wrote atomic.Bool
		trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }, WroteRequest: func(info httptrace.WroteRequestInfo) { wrote.Store(true) }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, opts.EnrollmentURL, bytes.NewReader(body))
		if err != nil {
			return nil, Response{}, fmt.Errorf("agent/enroll: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+opts.BearerSecret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				if opts.Retry && !wrote.Load() {
					state.Attempted = false
					_ = saveCheckpoint(opts, state)
				}
				return nil, Response{}, ctx.Err()
			}
			if !opts.Retry {
				return nil, Response{}, fmt.Errorf("agent/enroll: post: %w", err)
			}
			if wrote.Load() {
				return nil, Response{}, fmt.Errorf("%w: %v", ErrAmbiguousRedemption, err)
			}
			// No headers reached the connection: the token cannot have been spent.
			state.Attempted = false
			if saveErr := saveCheckpoint(opts, state); saveErr != nil {
				return nil, Response{}, saveErr
			}
			if attempt+1 == attempts {
				return nil, Response{}, fmt.Errorf("agent/enroll: post: %w", err)
			}
			if err := retryPause(ctx, attempt); err != nil {
				return nil, Response{}, err
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, Response{}, fmt.Errorf("%w: read response: %v", ErrAmbiguousRedemption, readErr)
		}
		if opts.Retry && resp.StatusCode == http.StatusTooManyRequests {
			// The controller rate limiter runs before redemption. Unlike a 5xx,
			// this status positively proves the single-use token was not consumed.
			state.Attempted = false
			if err := saveCheckpoint(opts, state); err != nil {
				return nil, Response{}, err
			}
			if attempt+1 < attempts {
				if err := retryPause(ctx, attempt); err != nil {
					return nil, Response{}, err
				}
				continue
			}
		}
		if resp.StatusCode/100 != 2 {
			return nil, Response{}, fmt.Errorf("%w: status %d: %s", ErrServer, resp.StatusCode, summariseBody(data))
		}
		var out Response
		if resp.StatusCode == http.StatusAccepted {
			var pending pendingRedeem
			if err := json.Unmarshal(data, &pending); err != nil {
				return nil, Response{}, fmt.Errorf("agent/enroll: parse response: %w", err)
			}
			if pending.Status != "pending" || !safePendingID.MatchString(pending.PendingID) || pending.PollToken == "" {
				return nil, Response{}, errors.New("agent/enroll: invalid pending receipt")
			}
			state.Pending = &pending
			if err := saveCheckpoint(opts, state); err != nil {
				return nil, Response{}, err
			}
			approved, err := waitForApproval(ctx, client, opts, pending)
			if err != nil {
				return nil, Response{}, err
			}
			out = *approved
		} else if err := json.Unmarshal(data, &out); err != nil {
			return nil, Response{}, fmt.Errorf("agent/enroll: parse response: %w", err)
		}
		state.Response = &out
		if err := saveCheckpoint(opts, state); err != nil {
			return nil, Response{}, err
		}
		return key, out, nil
	}
	return nil, Response{}, ErrServer
}

func retryPause(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(1<<attempt) * 250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Protect the empty staging file BEFORE writing private key or poll token.
// POSIX mode bits alone do not establish a private Windows DACL.
func writeCheckpoint(path string, data []byte) error {
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	defer file.Close()
	if err := restrictCheckpoint(tmpPath); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
