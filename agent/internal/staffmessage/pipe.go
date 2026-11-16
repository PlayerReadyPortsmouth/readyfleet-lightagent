package staffmessage

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

// ProtocolVersion is carried on every /v1/notify/next response.
const ProtocolVersion = 1

// MaxAckBytes bounds POST /v1/notify/ack; a valid ack is under 120 bytes.
const MaxAckBytes = 512

// MaxResponseBytes bounds what the companion reads from /v1/notify/next.
const MaxResponseBytes = 8 * 1024

// Authorize verifies the pipe client (exact CYP SID, active console
// session). nil refuses everything.
type Authorize func(*http.Request) error

// NextResponse is the body of a 200 from GET /v1/notify/next.
type NextResponse struct {
	Protocol int     `json:"protocol"`
	Message  Message `json:"message"`
}

// Handler serves the notify pipe. It has exactly two routes:
//
//	GET  /v1/notify/next  long-poll: 200 NextResponse, or 204 when none arrived
//	POST /v1/notify/ack   Ack JSON: 204 accepted, 404 unknown/expired id
//
// The client can collect a message and say whether it was shown. It cannot
// create a message, name a command, or affect anything else in the service.
func Handler(b *Broker, authorize Authorize) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if b == nil || authorize == nil || authorize(r) != nil {
			http.Error(w, "this Windows account cannot receive staff messages", http.StatusForbidden)
			return
		}
		if r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/notify/next":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			msg, ok := b.Next(r.Context())
			if !ok {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// The session may have changed while the poll waited: check the
			// client again before the text leaves the service.
			if authorize(r) != nil {
				b.Acknowledge(Ack{ID: msg.ID, Reason: ReasonSessionChanged})
				http.Error(w, "session changed", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(NextResponse{Protocol: ProtocolVersion, Message: msg}); err != nil {
				// Never reached the companion: do not wait out AckTimeout.
				b.Acknowledge(Ack{ID: msg.ID, Reason: ReasonDisplayFailed})
			}
		case "/v1/notify/ack":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxAckBytes+1))
			if err != nil || len(body) > MaxAckBytes {
				http.Error(w, "ack unreadable or too large", http.StatusBadRequest)
				return
			}
			ack, ok := parseAck(body)
			if !ok {
				http.Error(w, "malformed ack", http.StatusBadRequest)
				return
			}
			if !b.Acknowledge(ack) {
				http.Error(w, "no such message awaiting an answer", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func parseAck(body []byte) (Ack, bool) {
	var a Ack
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return Ack{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return Ack{}, false
	}
	if a.Validate() != nil {
		return Ack{}, false
	}
	return a, true
}

// DecodeNext parses a /v1/notify/next body on the companion side. The
// caller must Revalidate the message before rendering it, and ack an invalid
// one by its id (which is why this does not reject it).
func DecodeNext(body []byte) (Message, error) {
	var v NextResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return Message{}, err
	}
	if v.Protocol != ProtocolVersion {
		return Message{}, errUnsupportedProtocol
	}
	return v.Message, nil
}

type protocolError string

func (e protocolError) Error() string { return string(e) }

const errUnsupportedProtocol = protocolError("unsupported staff message protocol")
