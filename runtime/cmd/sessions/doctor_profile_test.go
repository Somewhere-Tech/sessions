package main

import (
	"testing"
	"time"
)

func TestParseDoctorCPUProfileArgs(t *testing.T) {
	duration, err := parseDoctorArgs([]string{"--cpu-profile", "30s"})
	if err != nil || duration != 30*time.Second {
		t.Fatalf("profile args = %s, %v", duration, err)
	}
	for _, args := range [][]string{{"--cpu-profile", "500ms"}, {"--cpu-profile"}, {"--other"}} {
		if _, err := parseDoctorArgs(args); err == nil {
			t.Fatalf("parseDoctorArgs(%q) succeeded", args)
		}
	}
}
