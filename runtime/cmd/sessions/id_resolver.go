package main

import "strings"

// idCandidate is the common input to every CLI id-prefix resolver. Commands
// may attach a human label, but identity and ambiguity are decided here so an
// eight-character id printed by a list command has the same meaning everywhere.
type idCandidate struct {
	id    string
	label string
}

func resolveIDPrefix(reference, kind, listCommand string, candidates []idCandidate) (string, bool, error) {
	reference = strings.TrimSpace(reference)
	for _, candidate := range candidates {
		if candidate.id == reference {
			return candidate.id, true, nil
		}
	}
	matches := make([]idCandidate, 0, 2)
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.id, reference) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0].id, true, nil
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	return "", false, ambiguousIDPrefix(reference, kind, listCommand, matches)
}

func ambiguousIDPrefix(reference, kind, listCommand string, matches []idCandidate) error {
	formatted := make([]string, 0, len(matches))
	for _, candidate := range matches {
		value := candidate.id
		if candidate.label != "" {
			value += " (" + candidate.label + ")"
		}
		formatted = append(formatted, value)
	}
	return fail(1, "ambiguous %s prefix %q; candidates: %s; use more characters from `%s`",
		kind, reference, strings.Join(formatted, ", "), listCommand)
}

func candidatesForSessions(a *app, sessions []session) []idCandidate {
	candidates := make([]idCandidate, 0, len(sessions))
	for _, current := range sessions {
		candidates = append(candidates, idCandidate{id: current.ID, label: a.sessionLabel(current)})
	}
	return candidates
}

func candidateIndex(id string, candidates []idCandidate) int {
	for index, candidate := range candidates {
		if candidate.id == id {
			return index
		}
	}
	return -1
}

func (a *app) resolveRecoverySessionIDs(source string, sourceSet bool, repair string, repairSet bool) (string, string, error) {
	if sourceSet && source == "" {
		return "", "", fail(1, "--source requires the ended source session id")
	}
	if repairSet && repair == "" {
		return "", "", fail(1, "--repair requires the existing live successor id")
	}
	var err error
	if sourceSet {
		source, err = a.resolveSessionID(source)
	}
	if err == nil && repairSet {
		repair, err = a.resolveSessionID(repair)
	}
	return source, repair, err
}

func labeledID(id string, values ...string) idCandidate {
	return idCandidate{id: id, label: strings.TrimSpace(strings.Join(values, " "))}
}
