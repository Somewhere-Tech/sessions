package main

import (
	"bytes"
	"testing"
	"time"

	pprofprofile "github.com/google/pprof/profile"
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

func TestTopCPUFramesAreSymbolizedAndLimited(t *testing.T) {
	profile := &pprofprofile.Profile{SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
	for index := 0; index < 12; index++ {
		function := &pprofprofile.Function{ID: uint64(index + 1), Name: "symbol-" + string(rune('a'+index))}
		location := &pprofprofile.Location{ID: uint64(index + 1), Line: []pprofprofile.Line{{Function: function}}}
		profile.Function = append(profile.Function, function)
		profile.Location = append(profile.Location, location)
		profile.Sample = append(profile.Sample, &pprofprofile.Sample{Location: []*pprofprofile.Location{location}, Value: []int64{int64(index + 1)}})
	}
	var encoded bytes.Buffer
	if err := profile.Write(&encoded); err != nil {
		t.Fatal(err)
	}
	frames, err := topCPUFrames(encoded.Bytes(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 10 || frames[0].Symbol != "symbol-l" || frames[0].FlatMS <= frames[9].FlatMS {
		t.Fatalf("top frames = %#v", frames)
	}
}
