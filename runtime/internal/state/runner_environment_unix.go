//go:build !windows

package state

func addPlatformRunnerEnvironment(map[string]string) {}

func setRunnerEnvironment(environment map[string]string, key, value string) {
	environment[key] = value
}
