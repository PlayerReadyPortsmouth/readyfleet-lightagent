package companionadapter

import (
	"errors"
	"github.com/google/uuid"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	"net/url"
)

// Approval puts only a pairing ID in the normal browser; the secret stays in
// native memory. ReadyApp must use the same launcher so approval and viewers
// share the normal browser profile, rather than the legacy hidden profile.
func OpenApprovalURL(raw string) error           { return openApproval(raw, openNormalBrowser) }
func OpenReadyAppURL() error                     { return openReadyApp(openNormalBrowser) }
func openReadyApp(open func(string) error) error { return open(companionclient.DefaultOrigin + "/") }
func openApproval(raw string, open func(string) error) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || "https://"+u.Host != companionclient.DefaultOrigin || u.User != nil || u.Fragment != "" || u.Path != "/solarbeam/companion/connect" || u.RawPath != "" {
		return errors.New("invalid ReadyApp approval URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 1 || len(query["pairing"]) != 1 {
		return errors.New("invalid approval query")
	}
	id := query.Get("pairing")
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return errors.New("invalid pairing identity")
	}
	return open(raw)
}
