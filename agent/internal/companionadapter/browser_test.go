package companionadapter

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venue"
)

func TestApprovalAndReadyAppUseSameNormalBrowserOrigin(t *testing.T) {
	var opened []string
	normal := func(raw string) error { opened = append(opened, raw); return nil }
	if err := openApproval(companionclient.DefaultOrigin+"/solarbeam/companion/connect?pairing="+pair, normal); err != nil {
		t.Fatal(err)
	}
	if err := openReadyApp(normal); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 {
		t.Fatal(opened)
	}
	a, _ := url.Parse(opened[0])
	b, _ := url.Parse(opened[1])
	if a.Scheme != b.Scheme || a.Host != b.Host || b.Path != "/" || b.RawQuery != "" {
		t.Fatal("profile origin mismatch", opened)
	}
}
func TestApprovalRejectsUntrustedAndAmbiguousURLs(t *testing.T) {
	for _, raw := range []string{
		"http://readyapp.player-ready.co.uk/solarbeam/companion/connect?pairing=" + pair,
		companionclient.DefaultOrigin + ".evil.test/solarbeam/companion/connect?pairing=" + pair,
		companionclient.DefaultOrigin + "/solarbeam/companion/connect?pairing=" + pair + "&pairing=" + pair,
		companionclient.DefaultOrigin + "/solarbeam/companion/connect?pairing=" + pair + "&secret=x",
		companionclient.DefaultOrigin + "/solarbeam/companion/connect?pairing=" + pair + "#fragment",
	} {
		if err := openApproval(raw, func(string) error { t.Fatal("opened rejected URL"); return nil }); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

// Check platform wiring too: a shared safe helper is ineffective if one caller
// accidentally passes the legacy hidden-browser launcher again.
func TestBYODControlsUseNormalBrowserHelper(t *testing.T) {
	for path, call := range map[string]string{

		"../../cmd/lightagent/tray.go": "runLightCompanion(ctx, machineID, client, solarbeamMgr, companionadapter.OpenReadyAppURL, tray)",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), call) {
			t.Fatal("normal browser integration missing", path)
		}
	}
}
func TestVenueDetailsDoNotRenderOpaqueIdentifiers(t *testing.T) {
	v := venue.Snapshot{MachineID: "machine-sentinel", CurrentSessionID: "session-sentinel", HostingRevision: "revision-sentinel", HostingError: "error-sentinel", Remote: venue.RemotePresence{Known: true, Count: 1, Viewers: []venue.RemoteViewer{{Key: "viewer-key-sentinel", Name: "Example Staff"}}}, Limitations: []string{"raw-diagnostic-sentinel"}}
	details := VenueDetails(v)
	for _, secret := range []string{v.MachineID, v.CurrentSessionID, v.HostingRevision, v.HostingError, v.Remote.Viewers[0].Key, v.Limitations[0]} {
		if strings.Contains(details, secret) {
			t.Fatal("opaque value rendered", secret)
		}
	}
	if !strings.Contains(details, "Example Staff") {
		t.Fatal("friendly name missing")
	}
}
