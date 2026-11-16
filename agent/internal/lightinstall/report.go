package lightinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"
)

const reportEndpoint = "https://readyapp.player-ready.co.uk/solarbeam-devices/install-failure"

var reportVersion = "dev"

// Redact removes bootstrap/steady-state credentials and local account names
// before either writing diagnostics or sending them off the device.
func Redact(s string) string {
	s = redactBlock(s)
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

func redactBlock(s string) string {
	for _, pattern := range []string{
		`(?i)-----BEGIN[\s\S]*`,
		`(?i)(rft_|rfk_|rfc_|rfp_)[A-Za-z0-9_-]+`,
		`(?i)Bearer\s+[^\s"',;]+`,
		`(?i)https?://[^\s"<>]+`,
		`(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`,
		`(?i)(?:[A-Z]:\\Users\\|/home/|/Users/)[^\\/\r\n"']+`,
		`(?i)(?:token|secret|password|authorization|api[_-]?key|user(?:name)?)\s*["']?\s*[:=]\s*["']?[^\r\n"',;]+`,
		`[A-Za-z0-9_-]{32,}`,
	} {
		s = regexp.MustCompile(pattern).ReplaceAllString(s, "[redacted]")
	}
	// Scrub complete credentials before names: replacing a username inside
	// a token first can split it into short fragments that evade the patterns.
	for _, value := range []string{os.Getenv("USERNAME"), os.Getenv("USER"), os.Getenv("USERPROFILE"), os.Getenv("HOME")} {
		if value != "" {
			s = regexp.MustCompile("(?i)"+regexp.QuoteMeta(value)).ReplaceAllString(s, "[user]")
		}
	}
	if u, err := user.Current(); err == nil {
		for _, v := range []string{u.Username, u.Name, u.HomeDir} {
			if v != "" {
				s = regexp.MustCompile("(?i)"+regexp.QuoteMeta(v)).ReplaceAllString(s, "[user]")
			}
		}
	}
	s = strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' && r != '\n' {
			return ' '
		}
		return r
	}, s)
	return s
}

func redactLines(lines []string) []string {
	if len(lines) == 0 {
		return []string{}
	}
	block := redactBlock(strings.Join(lines, "\n"))
	lines = strings.Split(block, "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	for i := range lines {
		if len(lines[i]) > 512 {
			lines[i] = lines[i][:512]
		}
	}
	return lines
}

type failureReport struct {
	InstallerVersion string   `json:"installerVersion"`
	OS               string   `json:"os"`
	Step             string   `json:"step"`
	ErrorCode        string   `json:"errorCode"`
	Message          string   `json:"message"`
	LastLogLines     []string `json:"lastLogLines"`
	DeviceName       string   `json:"deviceName"`
	PairingCodeID    string   `json:"pairingCodeId"`
}

// ReportFailure is best effort and strictly bounded. The report capability is
// unrelated to the spent pairing/enrollment secret; it authorizes diagnostics
// only. No destination URL, webhook, or staff identity comes from the payload.
func ReportFailure(ctx context.Context, p Payload, err error) {
	if p.FailureReportToken == "" || p.PairingCodeID == "" {
		return
	}
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	reportFailure(ctx, client, reportEndpoint, p, err)
	// The generated Linux bootstrap must not send a second generic report
	// after this installer has already exhausted its scoped reporting attempts.
	if marker := os.Getenv("READYFLEET_INSTALL_REPORT_SENT_FILE"); marker != "" {
		_ = os.WriteFile(marker, []byte("attempted\n"), 0600)
	}
}
func reportFailure(ctx context.Context, client *http.Client, endpoint string, p Payload, installErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	step, code := "setup", "setup_failed"
	lines := []string{}
	var failure *Failure
	if errors.As(installErr, &failure) {
		step, code = failure.Step, failure.Code
		if failure.Rollback != nil {
			step = "rollback"
		}
		if failure.LogLines != nil {
			lines = failure.LogLines
		}
	}
	stepMap := map[string]string{"stage": "verify", "connect": "connection", "setup": "unknown"}
	if mapped, ok := stepMap[step]; ok {
		step = mapped
	}
	codes := map[string]string{"preflight": "PREFLIGHT_FAILED", "verify": "INTEGRITY_FAILED", "enroll": "ENROLLMENT_FAILED", "credentials": "CREDENTIALS_FAILED", "startup": "STARTUP_FAILED", "start": "START_FAILED", "connection": "CONNECTION_FAILED", "unknown": "INSTALL_FAILED", "rollback": "ROLLBACK_FAILED"}
	code = codes[step]
	host, _ := os.Hostname()
	scrub := func(s string) string {
		for _, secret := range []string{p.FailureReportToken, p.EnrollmentToken, strings.TrimPrefix(p.EnrollmentToken, "rfc_")} {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[redacted]")
			}
		}
		return redactBlock(s)
	}
	if len(lines) > 0 {
		lines = redactLines(strings.Split(scrub(strings.Join(lines, "\n")), "\n"))
	}
	message := Redact(scrub(installErr.Error()))
	b, _ := json.Marshal(failureReport{reportVersion, reportOS(), step, code, message, lines, Redact(host), p.PairingCodeID})
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(1<<(attempt-1)) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+p.FailureReportToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			return
		}
	}
}
