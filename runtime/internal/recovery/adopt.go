package recovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

var strictProviderPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var providerInNamePattern = regexp.MustCompile(`(?i)[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}`)

type AdoptionOptions struct {
	ClaudeProjectsDir string
	CodexSessionsDir  string
}

type Adoption struct {
	Path              string   `json:"path"`
	HistoryID         string   `json:"historyId,omitempty"`
	PromptHistoryOnly bool     `json:"promptHistoryOnly,omitempty"`
	Tool              string   `json:"tool"`
	Cwd               string   `json:"cwd"`
	ProviderUUID      string   `json:"providerUuid"`
	Cmd               string   `json:"cmd"`
	Args              []string `json:"args"`
}

type AdoptResult struct {
	OK                  bool                `json:"ok"`
	Partial             bool                `json:"partial,omitempty"`
	LaneID              string              `json:"laneId"`
	Adoption            Adoption            `json:"adoption"`
	Warning             string              `json:"warning,omitempty"`
	MissingAnnotations  []AdoptAnnotation   `json:"missingAnnotations,omitempty"`
	Repair              *AdoptRepairRequest `json:"repair,omitempty"`
	SourceHistoryID     string              `json:"sourceHistoryId,omitempty"`
	SourceProvider      string              `json:"sourceProvider,omitempty"`
	DestinationProvider string              `json:"destinationProvider,omitempty"`
	Mode                string              `json:"mode,omitempty"`
	ImportedMessages    int                 `json:"importedMessages,omitempty"`
}

type AdoptAnnotation string

const (
	AdoptAnnotationCreated       AdoptAnnotation = "adopt-created"
	AdoptAnnotationProviderBound AdoptAnnotation = "provider-bound"
	AdoptAnnotationSourceLinked  AdoptAnnotation = "source-linked"
)

type AdoptRepairRequest struct {
	Target          string `json:"target"`
	HistoryID       string `json:"historyId,omitempty"`
	LaneID          string `json:"laneId"`
	SourceSessionID string `json:"sourceSessionId,omitempty"`
}

// ResolveAdoption turns one explicit path or provider UUID into a minimal
// create-with-resume request. It never guesses among multiple Claude matches
// and never returns an identity that cannot be bound to an on-disk source.
func ResolveAdoption(target string, options AdoptionOptions) (Adoption, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Adoption{}, errors.New("adopt target is required")
	}
	if strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Adoption{}, err
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~/"))
	}
	if info, err := os.Stat(target); err == nil {
		if !info.Mode().IsRegular() {
			return Adoption{}, fmt.Errorf("adopt target is not a regular file: %s", target)
		}
		return adoptionFromPath(target)
	} else if strings.ContainsRune(target, filepath.Separator) || strings.HasSuffix(target, ".jsonl") {
		return Adoption{}, fmt.Errorf("adopt source does not exist: %s", target)
	}
	if !strictProviderPattern.MatchString(target) {
		return Adoption{}, fmt.Errorf("provider-unbound: %q is neither a conversation JSONL nor a provider UUID", target)
	}

	claude, claudeErr := findClaudeByUUID(target, options.ClaudeProjectsDir)
	codex, codexErr := findCodexByUUID(target, options.CodexSessionsDir)
	if claudeErr != nil {
		return Adoption{}, claudeErr
	}
	if codexErr != nil {
		return Adoption{}, codexErr
	}
	if claude.Path != "" && codex.Path != "" {
		return Adoption{}, fmt.Errorf("provider UUID %s is ambiguous across Claude and Codex stores; pass an explicit path", target)
	}
	if claude.Path != "" {
		return claude, nil
	}
	if codex.Path != "" {
		return codex, nil
	}
	return Adoption{}, fmt.Errorf("provider-unbound: no conversation source exists for %s", target)
}

// ResolvePromptHistoryAdoption is the deliberately narrow fallback for a
// Claude history.jsonl record whose full conversation JSONL is no longer on
// disk. Both identity and cwd must already be present in the authenticated
// provider history record. It never searches for a similar workspace and
// never accepts either value directly from an interactive client.
func ResolvePromptHistoryAdoption(historyID, providerUUID, cwd string) (Adoption, error) {
	historyID = strings.TrimSpace(historyID)
	providerUUID = strings.TrimSpace(providerUUID)
	cwd = strings.TrimSpace(cwd)
	if !strings.HasPrefix(historyID, "provider-history:claude:") {
		return Adoption{}, errors.New("prompt-index continuation requires an exact Claude history id")
	}
	if !strictProviderPattern.MatchString(providerUUID) {
		return Adoption{}, errors.New("provider-unbound: prompt history has no valid Claude conversation id")
	}
	if !filepath.IsAbs(cwd) {
		return Adoption{}, errors.New("provider-unbound: prompt history has no absolute recorded workspace")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return Adoption{}, fmt.Errorf("recorded Claude workspace is unavailable: %w", err)
	}
	if !info.IsDir() {
		return Adoption{}, errors.New("recorded Claude workspace is not a directory")
	}
	argv := ledger.ResumeRecipeForProvider(string(state.ToolClaude), "claude", providerUUID)
	if len(argv) == 0 {
		return Adoption{}, errors.New("provider-unbound: no safe Claude resume recipe")
	}
	return Adoption{
		HistoryID: historyID, PromptHistoryOnly: true,
		Tool: string(state.ToolClaude), Cwd: cwd, ProviderUUID: providerUUID,
		Cmd: argv[0], Args: append([]string(nil), argv[1:]...),
	}, nil
}

func findClaudeByUUID(uuid, explicitRoot string) (Adoption, error) {
	root := explicitRoot
	if root == "" {
		var err error
		root, err = watch.ClaudeProjectsDir()
		if err != nil {
			return Adoption{}, err
		}
	}
	projects, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return Adoption{}, nil
	}
	if err != nil {
		return Adoption{}, fmt.Errorf("scan Claude projects: %w", err)
	}
	matches := make([]string, 0, 1)
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		resolution := watch.ResolveClaudeJSONL(filepath.Join(root, project.Name()), uuid)
		if resolution.Path != "" && filepath.Base(resolution.Path) == uuid+".jsonl" {
			matches = append(matches, resolution.Path)
		}
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		return Adoption{}, fmt.Errorf("provider UUID %s matches multiple Claude projects; pass an explicit path", uuid)
	}
	if len(matches) == 0 {
		return Adoption{}, nil
	}
	return adoptionFromPath(matches[0])
}

func findCodexByUUID(uuid, root string) (Adoption, error) {
	resolution := watch.ResolveCodexRolloutPath(watch.CodexResolveOptions{
		Args: []string{"resume", uuid}, SessionsDir: root,
	})
	if resolution.Path == "" {
		return Adoption{}, nil
	}
	return adoptionFromPath(resolution.Path)
}

func adoptionFromPath(path string) (Adoption, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Adoption{}, err
	}
	provider, cwd, codex, err := readConversationIdentity(absolute)
	if err != nil {
		return Adoption{}, err
	}
	if provider == "" || !strictProviderPattern.MatchString(provider) {
		return Adoption{}, fmt.Errorf("provider-unbound: cannot resolve provider UUID from %s", absolute)
	}
	if cwd == "" {
		return Adoption{}, fmt.Errorf("provider-unbound: cannot resolve cwd from %s", absolute)
	}
	tool := string(state.ToolClaude)
	cmd := "claude"
	if codex {
		tool = string(state.ToolCodex)
		cmd = "codex"
	} else {
		resolution := watch.ResolveClaudeJSONL(filepath.Dir(absolute), provider)
		if resolution.Path == "" || filepath.Clean(resolution.Path) != filepath.Clean(absolute) {
			return Adoption{}, fmt.Errorf("provider-unbound: Claude resolver did not bind %s", absolute)
		}
	}
	argv := ledger.ResumeRecipeForProvider(tool, cmd, provider)
	if len(argv) == 0 {
		return Adoption{}, fmt.Errorf("provider-unbound: no safe resume recipe for %s", provider)
	}
	return Adoption{
		Path: absolute, Tool: tool, Cwd: cwd, ProviderUUID: provider,
		Cmd: argv[0], Args: append([]string(nil), argv[1:]...),
	}, nil
}

func readConversationIdentity(path string) (provider, cwd string, codex bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for lines := 0; lines < 256 && scanner.Scan(); lines++ {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if value, ok := record["cwd"].(string); ok && cwd == "" {
			cwd = value
		}
		if record["type"] == "session_meta" {
			codex = true
			if payload, ok := record["payload"].(map[string]any); ok {
				if value, ok := payload["cwd"].(string); ok && cwd == "" {
					cwd = value
				}
				if value, ok := payload["id"].(string); ok {
					provider = value
				}
			}
		}
		if provider != "" && cwd != "" {
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return "", "", false, fmt.Errorf("read conversation %s: %w", path, scanErr)
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if !codex && strings.HasPrefix(base, "rollout-") {
		codex = true
	}
	if provider == "" {
		if codex {
			matches := providerInNamePattern.FindAllString(base, -1)
			if len(matches) > 0 {
				provider = matches[len(matches)-1]
			}
		} else {
			provider = base
		}
	}
	if cwd == "" && !codex {
		cwd = strings.ReplaceAll(filepath.Base(filepath.Dir(path)), "-", "/")
		if cwd != "" && !strings.HasPrefix(cwd, "/") {
			cwd = "/" + cwd
		}
	}
	return provider, cwd, codex, nil
}

type AdoptOptions struct {
	Force       bool
	Source      *AdoptSource
	Events      AdoptionEventReader
	RuntimeMode string
}

type AdoptionEventReader interface {
	Events(context.Context, string) ([]ledger.Event, error)
}

// AdoptSource is trusted metadata copied from an existing Sessions record
// when Resume is invoked from that record. It keeps display identity and
// organization stable without importing or rewriting provider history.
type AdoptSource struct {
	LaneID                 string
	Name                   string
	Description            string
	Tags                   map[string]string
	Profile                string
	ConfigDir              string
	Kind                   string
	WorktreePath           string
	Branch                 string
	SourceRepo             string
	DisplayParentSessionID *string
}

// Adopt creates through the normal manager boundary, then appends explicit
// actor=adopt facts. The normal created event remains the write-ahead record;
// the adopt-authored pair makes the user's explicit external adoption
// auditable without weakening that launch boundary.
func Adopt(
	ctx context.Context,
	adoption Adoption,
	name string,
	creator SessionCreator,
	boundaries ledger.BoundaryWriter,
	observations ledger.ObservationWriter,
	options ...AdoptOptions,
) (AdoptResult, error) {
	selected := AdoptOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	if adoption.ProviderUUID == "" || len(adoption.Args) == 0 {
		return AdoptResult{}, errors.New("provider-unbound: adoption has no safe provider resume recipe")
	}
	if selected.Source != nil && strings.TrimSpace(name) == "" {
		name = selected.Source.Name
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(adoption.Cwd)
	}
	description := ""
	var tags map[string]string
	profile := ""
	kind := ""
	var displayParent *string
	if selected.Source != nil {
		description = selected.Source.Description
		tags = state.CloneTags(selected.Source.Tags)
		profile = selected.Source.Profile
		kind = selected.Source.Kind
		if selected.Source.DisplayParentSessionID != nil {
			parent := *selected.Source.DisplayParentSessionID
			displayParent = &parent
		}
	}
	if adoption.Tool == string(state.ToolCodex) && kind == "" {
		kind = state.KindCodexAppServer
	}
	if adoption.Tool == string(state.ToolClaude) && kind == "" {
		kind = state.KindClaudeStructured
	}
	if selected.RuntimeMode == "terminal" {
		kind = ""
	} else if selected.RuntimeMode != "" && selected.RuntimeMode != "rich" {
		return AdoptResult{}, errors.New("runtime mode must be rich or terminal")
	}
	conversationID := ""
	if kind == state.KindCodexAppServer || kind == state.KindClaudeStructured {
		conversationID = adoption.ProviderUUID
	}
	created, err := creator.Create(ctx, state.CreateSessionRequest{
		Cmd: adoption.Cmd, Args: append([]string(nil), adoption.Args...),
		Cwd: adoption.Cwd, Name: name, Description: description, Tags: tags,
		Profile: profile, Kind: kind, ConversationID: conversationID,
		DisplayParentSessionID: displayParent, Force: selected.Force,
	})
	if err != nil {
		return AdoptResult{}, err
	}
	result := finishAdoptionAnnotations(
		ctx, adoption, name, description, kind, created.ID, selected.Source,
		selected.Events, boundaries, observations,
	)
	return result, nil
}

// RepairAdopt finishes only missing post-launch ledger annotations for an
// already-created successor. It never calls SessionCreator and therefore
// cannot launch a second runtime for the provider conversation.
func RepairAdopt(
	ctx context.Context,
	adoption Adoption,
	name string,
	laneID string,
	source *AdoptSource,
	events AdoptionEventReader,
	boundaries ledger.BoundaryWriter,
	observations ledger.ObservationWriter,
) (AdoptResult, error) {
	if strings.TrimSpace(laneID) == "" {
		return AdoptResult{}, errors.New("repair lane id is required")
	}
	if events == nil {
		return AdoptResult{}, errors.New("cannot repair adoption without the durable event reader")
	}
	laneEvents, err := events.Events(ctx, laneID)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("read successor ledger: %w", err)
	}
	states := ledger.Fold(laneEvents)
	if len(states) != 1 || !states[0].Created {
		return AdoptResult{}, fmt.Errorf("cannot repair adoption for %s: no durable creation record", laneID)
	}
	if states[0].ProviderUUID != adoption.ProviderUUID {
		return AdoptResult{}, fmt.Errorf(
			"cannot repair adoption for %s: provider identity does not match the live successor",
			laneID,
		)
	}
	if strings.TrimSpace(name) == "" {
		name = states[0].Name
	}
	return finishAdoptionAnnotations(
		ctx, adoption, name, states[0].Description, states[0].Kind, laneID,
		source, events, boundaries, observations,
	), nil
}

func finishAdoptionAnnotations(
	ctx context.Context,
	adoption Adoption,
	name string,
	description string,
	kind string,
	laneID string,
	source *AdoptSource,
	events AdoptionEventReader,
	boundaries ledger.BoundaryWriter,
	observations ledger.ObservationWriter,
) AdoptResult {
	base := AdoptResult{LaneID: laneID, Adoption: adoption}
	missing, readErr := adoptionMissingAnnotations(ctx, events, laneID, source)
	if events == nil {
		missing = requiredAdoptionAnnotations(source)
	}
	if readErr != nil {
		return markAdoptionPartial(
			base, source, missing, []string{fmt.Sprintf("inspect annotations: %v", readErr)},
		)
	}
	failures := make([]string, 0, len(missing))
	failedAnnotations := make([]AdoptAnnotation, 0, len(missing))
	resumeArgv := append([]string{adoption.Cmd}, adoption.Args...)
	for _, annotation := range missing {
		var err error
		switch annotation {
		case AdoptAnnotationCreated:
			if boundaries == nil {
				err = errors.New("creation annotation writer is unavailable")
				break
			}
			profile, configDir := "", ""
			if source != nil {
				profile, configDir = source.Profile, source.ConfigDir
			}
			creatorID, creatorErr := ledger.LocalUserCreatorID()
			if creatorErr != nil {
				err = creatorErr
				break
			}
			err = boundaries.RecordCreated(ctx, ledger.Created{
				Meta: ledger.Meta{LaneID: laneID, AtMS: time.Now().UnixMilli(), Actor: ledger.ActorAdopt},
				Name: name, Description: description, Kind: kind, Tool: adoption.Tool, Cwd: adoption.Cwd,
				Profile: profile, ConfigDir: configDir,
				ResumeArgv: resumeArgv, LaneUUID: laneID, ProviderUUID: adoption.ProviderUUID,
				CreatorKind: ledger.CreatorUser, CreatorID: creatorID,
			})
		case AdoptAnnotationProviderBound:
			if observations == nil {
				err = errors.New("provider annotation writer is unavailable")
				break
			}
			err = observations.RecordProviderBound(ctx, ledger.ProviderBound{
				Meta:         ledger.Meta{LaneID: laneID, Actor: ledger.ActorAdopt},
				ProviderUUID: adoption.ProviderUUID, ResumeArgv: resumeArgv,
			})
		case AdoptAnnotationSourceLinked:
			if observations == nil {
				err = errors.New("source-link annotation writer is unavailable")
				break
			}
			err = observations.RecordReopened(ctx, ledger.Reopened{
				Meta: ledger.Meta{LaneID: source.LaneID}, NewLaneID: laneID,
			})
		}
		if err != nil {
			failedAnnotations = append(failedAnnotations, annotation)
			failures = append(failures, fmt.Sprintf("%s: %v", annotation, err))
		}
	}

	finalMissing := failedAnnotations
	if events != nil {
		if observed, err := adoptionMissingAnnotations(ctx, events, laneID, source); err == nil {
			finalMissing = observed
		} else {
			finalMissing = observed
			failures = append(failures, fmt.Sprintf("verify annotations: %v", err))
		}
	}
	if len(finalMissing) == 0 && len(failures) == 0 {
		base.OK = true
		return base
	}
	if len(finalMissing) == 0 {
		// A writer can report a post-commit filesystem error even though the
		// append is durable. The re-read is authoritative and makes repair
		// idempotent rather than appending a duplicate fact.
		base.OK = true
		return base
	}
	return markAdoptionPartial(base, source, finalMissing, failures)
}

func markAdoptionPartial(
	base AdoptResult,
	source *AdoptSource,
	missing []AdoptAnnotation,
	failures []string,
) AdoptResult {
	base.Partial = true
	base.MissingAnnotations = missing
	sourceID := ""
	if source != nil {
		sourceID = source.LaneID
	}
	base.Repair = &AdoptRepairRequest{
		Target: base.Adoption.Path, HistoryID: base.Adoption.HistoryID,
		LaneID: base.LaneID, SourceSessionID: sourceID,
	}
	if base.Repair.Target == "" {
		base.Repair.Target = base.Adoption.ProviderUUID
	}
	base.Warning = fmt.Sprintf(
		"Session %s is running, but Sessions could not finish these adoption records: %s. Repair records only; do not Continue again.",
		base.LaneID, strings.Join(adoptAnnotationStrings(missing), ", "),
	)
	if len(failures) > 0 {
		base.Warning += " " + strings.Join(failures, "; ")
	}
	return base
}

func requiredAdoptionAnnotations(source *AdoptSource) []AdoptAnnotation {
	required := []AdoptAnnotation{AdoptAnnotationCreated, AdoptAnnotationProviderBound}
	if source != nil && source.LaneID != "" {
		required = append(required, AdoptAnnotationSourceLinked)
	}
	return required
}

func adoptionMissingAnnotations(
	ctx context.Context,
	events AdoptionEventReader,
	laneID string,
	source *AdoptSource,
) ([]AdoptAnnotation, error) {
	if events == nil {
		return nil, nil
	}
	laneEvents, err := events.Events(ctx, laneID)
	if err != nil {
		return requiredAdoptionAnnotations(source), err
	}
	adoptCreated, adoptBound := false, false
	for _, event := range laneEvents {
		adoptCreated = adoptCreated || event.Type == ledger.EventCreated && event.Actor == ledger.ActorAdopt
		adoptBound = adoptBound || event.Type == ledger.EventProviderBound && event.Actor == ledger.ActorAdopt
	}
	missing := make([]AdoptAnnotation, 0, 3)
	if !adoptCreated {
		missing = append(missing, AdoptAnnotationCreated)
	}
	if !adoptBound {
		missing = append(missing, AdoptAnnotationProviderBound)
	}
	if source != nil && source.LaneID != "" {
		sourceEvents, err := events.Events(ctx, source.LaneID)
		if err != nil {
			return append(missing, AdoptAnnotationSourceLinked), err
		}
		linked := false
		for _, state := range ledger.Fold(sourceEvents) {
			if state.LaneID != source.LaneID {
				continue
			}
			if state.ReopenedAs != "" && state.ReopenedAs != laneID {
				return append(missing, AdoptAnnotationSourceLinked), fmt.Errorf(
					"source session is already linked to successor %s", state.ReopenedAs,
				)
			}
			linked = state.ReopenedAs == laneID
		}
		if !linked {
			missing = append(missing, AdoptAnnotationSourceLinked)
		}
	}
	return missing, nil
}

func adoptAnnotationStrings(values []AdoptAnnotation) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}
