// Package cpuprofile turns a Go CPU profile into the few lines a person needs.
//
// `sessions doctor --cpu-profile` has printed a top-frames table since profiles
// were reachable at all. The daemon now needs the same summary for a burst it
// profiles itself, and two summarisers that disagree about what "41%" means
// would be worse than none: this is the one both use.
package cpuprofile

import (
	"fmt"
	"sort"
	"strings"
	"time"

	pprofprofile "github.com/google/pprof/profile"
)

// Frame is one leaf symbol's share of a profile.
type Frame struct {
	Symbol  string  `json:"symbol"`
	FlatMS  float64 `json:"flat_ms"`
	Percent float64 `json:"percent"`
}

// TopFrames is where the CPU went, leaf-first, largest share first.
func TopFrames(encoded []byte, limit int) ([]Frame, error) {
	profile, err := pprofprofile.ParseData(encoded)
	if err != nil {
		return nil, err
	}
	valueIndex := valueIndexOf(profile)
	values := make(map[string]int64)
	var total int64
	for _, sample := range profile.Sample {
		if valueIndex >= len(sample.Value) || len(sample.Location) == 0 {
			continue
		}
		value := sample.Value[valueIndex]
		total += value
		values[leafSymbol(sample.Location[0])] += value
	}
	frames := make([]Frame, 0, len(values))
	for symbol, value := range values {
		percent := 0.0
		if total > 0 {
			percent = float64(value) * 100 / float64(total)
		}
		frames = append(frames, Frame{Symbol: symbol, FlatMS: float64(value) / 1e6, Percent: percent})
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].FlatMS > frames[j].FlatMS })
	return frames[:min(limit, len(frames))], nil
}

// Line is the one-line form: "a 41%, b 22%, c 9%". It carries symbol names and
// percentages and nothing else — no paths, no arguments, nothing about anybody's
// work — so it can be logged on a production machine and pasted into a report.
func Line(frames []Frame) string {
	parts := make([]string, 0, len(frames))
	for _, frame := range frames {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", ShortSymbol(frame.Symbol), frame.Percent))
	}
	return strings.Join(parts, ", ")
}

// ShortSymbol drops the module path a reader already knows.
func ShortSymbol(symbol string) string {
	if index := strings.LastIndex(symbol, "/"); index >= 0 && index+1 < len(symbol) {
		return symbol[index+1:]
	}
	return symbol
}

// Duration is a frame's flat time, rounded for reading.
func (f Frame) Duration() time.Duration {
	return (time.Duration(f.FlatMS * 1e6)).Round(time.Millisecond)
}

func valueIndexOf(profile *pprofprofile.Profile) int {
	for index, sampleType := range profile.SampleType {
		if sampleType.Type == "cpu" || sampleType.Unit == "nanoseconds" {
			return index
		}
	}
	return max(0, len(profile.SampleType)-1)
}

func leafSymbol(location *pprofprofile.Location) string {
	for _, line := range location.Line {
		if line.Function != nil && line.Function.Name != "" {
			return line.Function.Name
		}
	}
	return fmt.Sprintf("0x%x", location.Address)
}
