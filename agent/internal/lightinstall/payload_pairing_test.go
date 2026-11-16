package lightinstall

import "testing"

// The unified installer redeems an rfc_ pairing code; the composed per-person
// download still redeems an rft_ token. Both must pass the shape check, and
// nothing else may.
func TestParsePayload_AcceptsPairingCodeAndTokenShapes(t *testing.T) {
	ok := []string{
		`{"enrollment_token":"rfc_ABCDEFGHJ"}`,
		`{"enrollment_token":"rft_` + string(make43()) + `"}`,
	}
	for _, body := range ok {
		if _, err := ParsePayload([]byte(body)); err != nil {
			t.Errorf("%s rejected: %v", body, err)
		}
	}
	bad := []string{
		`{"enrollment_token":"rfk_ABCDEFGHJ"}`, // a venue key is never a light credential
		`{"enrollment_token":"rfc_ABCDEFGH"}`,  // 8 chars
		`{"enrollment_token":"rfc_ABCDEFGHI"}`, // I is not in the pairing alphabet
		`{"enrollment_token":"rfc_abcdefghj"}`, // lower case is normalised before this point
		`{"enrollment_token":"ABCDEFGHJ"}`,
	}
	for _, body := range bad {
		if _, err := ParsePayload([]byte(body)); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
}

func make43() []byte {
	b := make([]byte, 43)
	for i := range b {
		b[i] = 'a'
	}
	return b
}
