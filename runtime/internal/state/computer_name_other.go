//go:build !darwin && !linux && !windows

package state

func platformComputerName() string { return "" }
