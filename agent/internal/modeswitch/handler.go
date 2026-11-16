package modeswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// MaxRequestBytes bounds POST /v1/mode/request. The largest valid body,
// {"target_mode":"ap_clubs","pin":"0000"}, is under 40 bytes.
const MaxRequestBytes = 128

// MaxResponseBytes bounds what the companion reads back.
const MaxResponseBytes = 64 * 1024

// Authorize verifies the pipe client (exact CYP SID, interactive session)
// before any route runs. nil refuses everything.
type Authorize func(*http.Request) error

// Handler serves the mode pipe. It has exactly two routes:
//
//	GET  /v1/mode/status   the Status view
//	POST /v1/mode/request  {"target_mode":"ap_clubs"|"vr","pin":"dddd"}
//
// The request body can name nothing else: no command, path, process,
// grant or list. Unknown fields, other modes, anything but four digits,
// oversized bodies and a second concurrent request are refused.
func Handler(svc *Service, authorize Authorize, runCtx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if authorize == nil || authorize(r) != nil {
			http.Error(w, "this Windows account cannot use the mode picker", http.StatusForbidden)
			return
		}
		if r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/mode/status":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, http.StatusOK, svc.Status())
		case "/v1/mode/request":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleRequest(w, r, svc, runCtx)
		default:
			http.NotFound(w, r)
		}
	})
}

func handleRequest(w http.ResponseWriter, r *http.Request, svc *Service, runCtx context.Context) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	var buf [MaxRequestBytes + 1]byte
	defer clear(buf[:])
	n, err := io.ReadFull(io.LimitReader(r.Body, MaxRequestBytes+1), buf[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		http.Error(w, "request unreadable", http.StatusBadRequest)
		return
	}
	if n > MaxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	mode, pin, ok := parseRequest(buf[:n])
	defer clear(pin[:])
	if !ok {
		http.Error(w, "send only target_mode (ap_clubs or vr) and a four-digit pin", http.StatusBadRequest)
		return
	}
	res := svc.Request(r.Context(), runCtx, mode, &pin)
	status := http.StatusOK
	if res.Code == CodeBusy {
		status = http.StatusConflict
	}
	writeJSON(w, status, res)
}

// parseRequest accepts exactly {"target_mode": <picker mode>, "pin": "dddd"}.
// The PIN is copied out of a json.RawMessage into a fixed array; the raw
// bytes are wiped. (The decoder's internal buffer is a GC-owned copy.)
func parseRequest(body []byte) (string, [proto.OperatingModePINDigits]byte, bool) {
	var pin [proto.OperatingModePINDigits]byte
	var v struct {
		TargetMode *string          `json:"target_mode"`
		PIN        *json.RawMessage `json:"pin"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	err := dec.Decode(&v)
	if v.PIN != nil {
		defer clear(*v.PIN)
	}
	if err != nil || dec.More() || v.TargetMode == nil || v.PIN == nil {
		return "", pin, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", pin, false
	}
	raw := *v.PIN
	if !proto.IsPickerOperatingMode(*v.TargetMode) || len(raw) != proto.OperatingModePINDigits+2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", pin, false
	}
	for i := 0; i < proto.OperatingModePINDigits; i++ {
		c := raw[i+1]
		if c < '0' || c > '9' {
			clear(pin[:])
			return "", pin, false
		}
		pin[i] = c
	}
	return *v.TargetMode, pin, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
