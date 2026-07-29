//go:build windows

package state

import (
	"os"
	"strings"
)

// addPlatformRunnerEnvironment preserves the signed-in user's standard
// Windows environment. Provider CLIs rely on these paths for credentials,
// settings, temporary files, child-process lookup, and the Windows SDK.
func addPlatformRunnerEnvironment(environment map[string]string) {
	for _, key := range []string{
		"APPDATA",
		"COMSPEC",
		"HOMEDRIVE",
		"HOMEPATH",
		"LOCALAPPDATA",
		"OneDrive",
		"PATHEXT",
		"ProgramData",
		"ProgramFiles",
		"ProgramFiles(x86)",
		"SystemDrive",
		"SystemRoot",
		"TEMP",
		"TMP",
		"USERDOMAIN",
		"USERNAME",
		"USERPROFILE",
		"WINDIR",
	} {
		if value := os.Getenv(key); value != "" {
			setRunnerEnvironment(environment, key, value)
		}
	}
}

func setRunnerEnvironment(environment map[string]string, key, value string) {
	for existing := range environment {
		if strings.EqualFold(existing, key) {
			environment[existing] = value
			return
		}
	}
	environment[key] = value
}
