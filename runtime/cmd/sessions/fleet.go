package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	fleetRequestTimeout      = 5 * time.Second
	localFleetRequestTimeout = 60 * time.Second
)

type fleetTarget struct {
	Alias    string
	Name     string
	Endpoint string
	Client   *apiClient
	Owned    bool
}

func fleetTargetTimeout(target fleetTarget) time.Duration {
	if target.Endpoint == "local" {
		// The first local search may need to build or upgrade a sizeable index.
		// Remote peers stay tightly bounded so one stale machine cannot stall the fleet.
		return localFleetRequestTimeout
	}
	return fleetRequestTimeout
}

func qualifiedHistoryReference(alias, historyID string) string {
	return alias + "::" + historyID
}

func splitQualifiedHistoryReference(value string) (string, string, bool) {
	alias, historyID, ok := strings.Cut(strings.TrimSpace(value), "::")
	alias = strings.TrimSpace(alias)
	historyID = strings.TrimSpace(historyID)
	return alias, historyID, ok && alias != "" && historyID != ""
}

func (a *app) useQualifiedHistoryReference(value *string) (string, error) {
	alias, historyID, qualified := splitQualifiedHistoryReference(*value)
	if !qualified {
		return "", nil
	}
	var client *apiClient
	var err error
	if strings.EqualFold(alias, "local") || strings.EqualFold(alias, "this-machine") {
		tokenPath, pathErr := sessionstate.LocalTokenPathFromEnv()
		if pathErr != nil {
			return "", pathErr
		}
		client, err = newAPIClient("127.0.0.1", a.port, tokenPath, true)
		alias = "local"
	} else {
		machine, machineErr := loadSavedMachine(a.home, alias)
		if machineErr != nil {
			return "", machineErr
		}
		client, err = newAPIClient(
			machine.Endpoint, "", savedMachineTokenPath(a.home, machine.MachineID), false,
		)
		alias = machine.Alias
	}
	if err != nil {
		return "", err
	}
	if a.api != nil {
		a.api.close()
	}
	a.api = client
	*value = historyID
	return alias, nil
}

func (a *app) approvedFleetTargets() ([]fleetTarget, error) {
	targets := []fleetTarget{{Alias: "local", Name: "This machine", Endpoint: "local", Client: a.api}}
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return nil, err
	}
	for _, machine := range registry.Machines {
		client, clientErr := newAPIClient(
			machine.Endpoint, "", savedMachineTokenPath(a.home, machine.MachineID), false,
		)
		if clientErr != nil {
			continue
		}
		targets = append(targets, fleetTarget{
			Alias: machine.Alias, Name: machine.Name, Endpoint: machine.Endpoint,
			Client: client, Owned: true,
		})
	}
	return targets, nil
}

func getJSONFromClient(client *apiClient, path string, target any, timeout time.Duration) error {
	response, err := client.request(context.Background(), http.MethodGet, path, nil, timeout)
	if err != nil {
		return err
	}
	if response.status >= 400 {
		return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	return json.Unmarshal(response.body, target)
}
