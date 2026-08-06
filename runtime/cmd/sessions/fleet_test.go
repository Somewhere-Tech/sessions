package main

import "testing"

func TestFleetTargetTimeoutAllowsLocalIndexBuild(t *testing.T) {
	local := fleetTargetTimeout(fleetTarget{Endpoint: "local"})
	remote := fleetTargetTimeout(fleetTarget{Endpoint: "https://mini.example"})
	if local <= remote {
		t.Fatalf("local timeout = %s, remote = %s; local index build needs the larger budget", local, remote)
	}
}
