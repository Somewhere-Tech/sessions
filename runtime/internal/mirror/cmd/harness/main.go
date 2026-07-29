// Command harness emits the Go mirror result for one recorded PTY stream.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/somewhere-tech/sessions/runtime/internal/mirror"
)

type result struct {
	Snapshot          string `json:"snapshot"`
	Serialized        string `json:"serialized"`
	RoundTripSnapshot string `json:"roundTripSnapshot"`
	Reflow            string `json:"reflow"`
}

func main() {
	input := flag.String("input", "", "raw PTY recording")
	reflowWidth := flag.Int("reflow", 60, "reflow width")
	flag.Parse()
	if *input == "" {
		fmt.Fprintln(os.Stderr, "-input is required")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*input)
	check(err)
	m := mirror.New()
	_, err = m.Write(raw)
	check(err)

	got := result{
		Snapshot:   m.Snapshot(),
		Serialized: m.SerializeANSI(),
		Reflow:     m.ReflowTo(*reflowWidth),
	}
	check(m.Close())

	clone := mirror.New()
	_, err = clone.Write([]byte(got.Serialized))
	check(err)
	got.RoundTripSnapshot = clone.Snapshot()
	check(clone.Close())

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	check(enc.Encode(got))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
