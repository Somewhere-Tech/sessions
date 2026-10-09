package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type providerUpdateResult struct {
	Provider providerStatus `json:"provider"`
	Output   string         `json:"output"`
}

type machineProviderUpdate struct {
	Machine  string          `json:"machine"`
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Provider *providerStatus `json:"provider,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

func updateProviderOnClient(client *apiClient, provider string) (providerUpdateResult, error) {
	var result providerUpdateResult
	response, err := client.request(context.Background(), http.MethodPost, "/api/providers/"+provider+"/update", map[string]any{}, 5*time.Minute+20*time.Second)
	if err != nil {
		return result, fail(2, "update confirmation unavailable: %s; check the installed version before trying again, since the installer may still be running", err)
	}
	if response.status >= 400 {
		return result, fail(2, "%s", apiErrorMessage(response.body))
	}
	if err := json.Unmarshal(response.body, &result); err != nil {
		return result, err
	}
	if result.Provider.ID != provider || !result.Provider.Installed {
		return result, fail(2, "update returned no usable provider result; check the installed version before trying again")
	}
	return result, nil
}

func (a *app) updateFleetProvider(provider string) error {
	if a.explicitTarget {
		return fail(1, "--all updates this computer and its paired fleet; omit --machine, --host and --port, or update the selected computer without --all")
	}
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return err
	}
	results := []machineProviderUpdate{{Machine: "local", Name: "This computer", Status: "waiting"}}
	clients := []*apiClient{a.api}
	for _, machine := range registry.Machines {
		result := machineProviderUpdate{Machine: machine.Alias, Name: machine.Name, Status: "waiting"}
		var client *apiClient
		if a.direct {
			client, err = newAPIClient(machine.Endpoint, "", savedMachineTokenPath(a.home, machine.MachineID), false)
		} else {
			client, err = a.api.withFleetRelay(machine)
		}
		if err != nil {
			result.Status, result.Detail = "unknown", err.Error()
		}
		results, clients = append(results, result), append(clients, client)
		if client != nil {
			defer client.close()
		}
	}
	jobs := make(chan int, len(clients))
	for index := range clients {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for count := 0; count < 2; count++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if clients[index] == nil {
					continue
				}
				result, updateErr := updateProviderOnClient(clients[index], provider)
				if updateErr != nil {
					results[index].Status, results[index].Detail = "unknown", updateErr.Error()
				} else {
					results[index].Status, results[index].Provider = "updated", &result.Provider
				}
			}
		}()
	}
	workers.Wait()
	complete := true
	for _, result := range results {
		complete = complete && result.Status == "updated"
	}
	if a.wantJSON {
		if err := writeJSON(a.stdout, map[string]any{"complete": complete, "results": results}, true); err != nil {
			return err
		}
	} else {
		for _, result := range results {
			detail := result.Detail
			if result.Provider != nil {
				detail = result.Provider.Version
			}
			fmt.Fprintf(a.stdout, "%s: %s %s\n", result.Name, result.Status, detail)
		}
		fmt.Fprintln(a.stdout, "Running sessions keep their existing process; new sessions use each machine's installed version.")
	}
	if !complete {
		return status(2)
	}
	return nil
}
