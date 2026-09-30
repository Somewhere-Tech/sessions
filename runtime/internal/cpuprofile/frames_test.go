package cpuprofile

import (
	"bytes"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"
)

// A profile is only useful if the answer fits in a sentence: which symbol, what
// share. This moved here from `sessions doctor` when the daemon started
// profiling its own bursts — one summariser, so the two never disagree.
func TestTopFramesAreSymbolizedAndLimited(t *testing.T) {
	frames, err := TopFrames(syntheticProfile(t, 12), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 10 || frames[0].Symbol != "symbol-l" || frames[0].FlatMS <= frames[9].FlatMS {
		t.Fatalf("top frames = %#v", frames)
	}
}

// The line the daemon logs during a burst: symbols and percentages, nothing
// that belongs to anybody's work.
func TestLineNamesTheDominantFramesAndNothingElse(t *testing.T) {
	frames, err := TopFrames(syntheticProfile(t, 3), 3)
	if err != nil {
		t.Fatal(err)
	}
	line := Line(frames)
	if !strings.Contains(line, "symbol-c") || !strings.Contains(line, "%") {
		t.Fatalf("line = %q, want the dominant symbol and its share", line)
	}
	if strings.Contains(line, "/") || strings.Contains(line, "\n") {
		t.Fatalf("line = %q, want one line of short symbols", line)
	}
}

func TestShortSymbolDropsTheModulePath(t *testing.T) {
	if got := ShortSymbol("github.com/somewhere-tech/sessions/runtime/internal/api.(*Server).ServeHTTP"); got != "api.(*Server).ServeHTTP" {
		t.Fatalf("short symbol = %q", got)
	}
}

func syntheticProfile(t *testing.T, symbols int) []byte {
	t.Helper()
	profile := &pprofprofile.Profile{SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
	for index := range symbols {
		function := &pprofprofile.Function{ID: uint64(index + 1), Name: "symbol-" + string(rune('a'+index))}
		location := &pprofprofile.Location{ID: uint64(index + 1), Line: []pprofprofile.Line{{Function: function}}}
		profile.Function = append(profile.Function, function)
		profile.Location = append(profile.Location, location)
		profile.Sample = append(profile.Sample, &pprofprofile.Sample{
			Location: []*pprofprofile.Location{location}, Value: []int64{int64(index+1) * 1_000_000},
		})
	}
	var encoded bytes.Buffer
	if err := profile.Write(&encoded); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
