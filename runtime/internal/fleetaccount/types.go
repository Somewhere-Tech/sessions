package fleetaccount

import "time"

type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	SessionToken string `json:"session_token,omitempty"`
}

type Endpoints struct {
	LAN       string `json:"lan,omitempty"`
	Tailnet   string `json:"tailnet,omitempty"`
	TailnetIP string `json:"tailnet_ip,omitempty"`
	Relay     string `json:"relay,omitempty"`
}

type Registration struct {
	MachineID        string    `json:"machine_id"`
	Name             string    `json:"name"`
	MachinePublicKey string    `json:"machine_public_key"`
	EndpointsJSON    Endpoints `json:"endpoints_json"`
	DaemonVersion    string    `json:"daemon_version"`
}

type Status struct {
	SignedIn              bool   `json:"signed_in"`
	User                  *User  `json:"user,omitempty"`
	MachinePublicKey      string `json:"machine_public_key,omitempty"`
	LastRegistrationAt    string `json:"last_registration_at,omitempty"`
	LastRegistrationError string `json:"last_registration_error,omitempty"`
	LastHeartbeatAt       string `json:"last_heartbeat_at,omitempty"`
}

type persistedState struct {
	Version               int       `json:"version"`
	Tokens                TokenPair `json:"tokens"`
	User                  User      `json:"user"`
	LastRegistrationAt    string    `json:"last_registration_at,omitempty"`
	LastRegistrationError string    `json:"last_registration_error,omitempty"`
	LastHeartbeatAt       string    `json:"last_heartbeat_at,omitempty"`
}

func (s persistedState) signedIn() bool {
	return s.Tokens.AccessToken != "" && s.Tokens.RefreshToken != ""
}

func registrationTime(now time.Time) string {
	return now.UTC().Format(time.RFC3339)
}
