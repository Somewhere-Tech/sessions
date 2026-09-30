//go:build !darwin

package localnetwork

func platformDenied(error) bool { return false }
