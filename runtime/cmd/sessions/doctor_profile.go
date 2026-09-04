package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pprofprofile "github.com/google/pprof/profile"
)

type cpuProfileFrame struct {
	Symbol  string  `json:"symbol"`
	FlatMS  float64 `json:"flat_ms"`
	Percent float64 `json:"percent"`
}

type cpuProfileReport struct {
	Path   string            `json:"path"`
	Frames []cpuProfileFrame `json:"top_frames"`
}

func parseDoctorArgs(args []string) (time.Duration, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if len(args) != 2 || args[0] != "--cpu-profile" {
		return 0, fail(1, "usage: sessions doctor [--cpu-profile DURATION]")
	}
	duration, err := time.ParseDuration(args[1])
	if err != nil || duration < time.Second || duration > 5*time.Minute || duration%time.Second != 0 {
		return 0, fail(1, "--cpu-profile must be a whole-second duration from 1s through 5m")
	}
	return duration, nil
}

func (a *app) captureCPUProfile(deep any, duration time.Duration) (*cpuProfileReport, error) {
	address := pprofAddress(deep)
	if address == "" {
		return nil, fail(2, "sessionsd CPU profiling is off; restart the daemon with SESSIONS_PPROF=127.0.0.1:6060")
	}
	if !profileAddressIsLoopback(address) {
		return nil, fail(2, "sessionsd reported a non-loopback pprof address; refusing to connect")
	}
	target := url.URL{Scheme: "http", Host: address, Path: "/debug/pprof/profile"}
	query := target.Query()
	query.Set("seconds", fmt.Sprint(int(duration/time.Second)))
	target.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fail(2, "capture sessionsd CPU profile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fail(2, "capture sessionsd CPU profile: pprof returned %s", response.Status)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 128*1024*1024))
	if err != nil {
		return nil, fail(2, "read sessionsd CPU profile: %v", err)
	}
	frames, err := topCPUFrames(encoded, 10)
	if err != nil {
		return nil, fail(2, "decode sessionsd CPU profile: %v", err)
	}
	path, err := writeCPUProfile(encoded)
	if err != nil {
		return nil, fail(2, "write sessionsd CPU profile: %v", err)
	}
	return &cpuProfileReport{Path: path, Frames: frames}, nil
}

func (a *app) requestedCPUProfile(deep any, duration time.Duration, local bool) (*cpuProfileReport, error) {
	if duration == 0 {
		return nil, nil
	}
	if !local {
		return nil, fail(2, "--cpu-profile is available only for a local sessionsd")
	}
	return a.captureCPUProfile(deep, duration)
}

func pprofAddress(deep any) string {
	root, _ := deep.(map[string]any)
	profile, _ := root["pprof"].(map[string]any)
	enabled, _ := profile["enabled"].(bool)
	address, _ := profile["address"].(string)
	if !enabled {
		return ""
	}
	return address
}

func profileAddressIsLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeCPUProfile(encoded []byte) (string, error) {
	name := fmt.Sprintf("sessionsd-cpu-%s-%d.pprof", time.Now().Format("20060102T150405"), os.Getpid())
	path, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return "", err
	}
	return path, file.Close()
}

func topCPUFrames(encoded []byte, limit int) ([]cpuProfileFrame, error) {
	profile, err := pprofprofile.ParseData(encoded)
	if err != nil {
		return nil, err
	}
	valueIndex := cpuValueIndex(profile)
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
	frames := make([]cpuProfileFrame, 0, len(values))
	for symbol, value := range values {
		percent := 0.0
		if total > 0 {
			percent = float64(value) * 100 / float64(total)
		}
		frames = append(frames, cpuProfileFrame{Symbol: symbol, FlatMS: float64(value) / 1e6, Percent: percent})
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].FlatMS > frames[j].FlatMS })
	return frames[:min(limit, len(frames))], nil
}

func cpuValueIndex(profile *pprofprofile.Profile) int {
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

func writeCPUProfileReport(writer io.Writer, report *cpuProfileReport) {
	if report == nil {
		return
	}
	fmt.Fprintf(writer, "CPU profile: %s\n", report.Path)
	fmt.Fprintln(writer, "Top 10 CPU frames:")
	for _, frame := range report.Frames {
		fmt.Fprintf(writer, "  %8s %6.2f%%  %s\n",
			(time.Duration(math.Round(frame.FlatMS*1e6)) * time.Nanosecond).Round(time.Millisecond),
			frame.Percent, frame.Symbol)
	}
	fmt.Fprintln(writer)
}
