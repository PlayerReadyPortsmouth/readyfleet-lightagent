package main

import (
	"errors"
	"strings"
	"testing"
)

func TestFinishLightUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, failure, wantCalls, wantError string
	}{
		{"replacement starts", "", "wait,start,remove", ""},
		{"parent still running", "wait", "wait", "wait for updating agent"},
		{"replacement fails", "start", "wait,start,rollback", "previous agent restored and relaunched"},
		{"rollback fails", "rollback", "wait,start,rollback", "rollback:"},
		{"backup cleanup fails", "remove", "wait,start,remove", "replacement running; retain backup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			op := func(name string) func() error {
				return func() error {
					calls = append(calls, name)
					if tc.failure == name || (tc.failure == "rollback" && name == "start") {
						return errors.New(name + " failed")
					}
					return nil
				}
			}
			err := finishLightUpdate(op("wait"), op("start"), op("rollback"), op("remove"))
			if got := strings.Join(calls, ","); got != tc.wantCalls {
				t.Errorf("calls=%s want=%s", got, tc.wantCalls)
			}
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error=%v want containing %q", err, tc.wantError)
			}
		})
	}
}
