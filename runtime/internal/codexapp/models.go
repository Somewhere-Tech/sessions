package codexapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Model is one entry in the live app-server model catalog.
type Model struct {
	ID                        string                  `json:"id"`
	Model                     string                  `json:"model"`
	DisplayName               string                  `json:"displayName"`
	Description               string                  `json:"description"`
	Hidden                    bool                    `json:"hidden"`
	IsDefault                 bool                    `json:"isDefault"`
	DefaultReasoningEffort    string                  `json:"defaultReasoningEffort"`
	DefaultServiceTier        *string                 `json:"defaultServiceTier,omitempty"`
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supportedReasoningEfforts"`
	ServiceTiers              []ModelServiceTier      `json:"serviceTiers"`
}

type ReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}

type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ModelChoice is a declared per-session model requirement. Empty effort and
// service-tier fields leave those settings at the selected model's defaults.
type ModelChoice struct {
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	ServiceTier string `json:"serviceTier,omitempty"`
}

type modelListParams struct {
	Cursor        *string `json:"cursor,omitempty"`
	IncludeHidden bool    `json:"includeHidden"`
}

type modelListResponse struct {
	Data       []Model `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

// ListModels returns every page from the live app-server catalog, including
// entries hidden from the default picker.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var models []Model
	var cursor *string
	seenCursors := make(map[string]struct{})
	for {
		var response modelListResponse
		if err := c.call(ctx, "model/list", modelListParams{Cursor: cursor, IncludeHidden: true}, &response); err != nil {
			return nil, fmt.Errorf("list Codex models: %w", err)
		}
		models = append(models, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return models, nil
		}
		if _, repeated := seenCursors[*response.NextCursor]; repeated {
			return nil, fmt.Errorf("list Codex models: server repeated pagination cursor %q", *response.NextCursor)
		}
		seenCursors[*response.NextCursor] = struct{}{}
		cursor = response.NextCursor
	}
}

// ResolveModelChoice validates an exact declared choice against the live
// catalog. It never substitutes an unavailable model, effort, or service tier.
// If no model is declared, the catalog default is selected so effort/tier
// validation cannot race against an implicit, unknown model choice.
func ResolveModelChoice(catalog []Model, choice ModelChoice) (ModelChoice, error) {
	if len(catalog) == 0 {
		return ModelChoice{}, errors.New("Codex model catalog is empty")
	}
	requestedModel := strings.TrimSpace(choice.Model)
	choice.Effort = strings.TrimSpace(choice.Effort)
	choice.ServiceTier = strings.TrimSpace(choice.ServiceTier)

	var selected *Model
	if requestedModel != "" {
		for index := range catalog {
			if catalog[index].ID == requestedModel {
				selected = &catalog[index]
				break
			}
		}
		if selected == nil {
			return ModelChoice{}, fmt.Errorf("model %q not available; valid: [%s]", requestedModel, strings.Join(modelIDs(catalog), ", "))
		}
	} else {
		for index := range catalog {
			if catalog[index].IsDefault {
				selected = &catalog[index]
				break
			}
		}
		if selected == nil {
			return ModelChoice{}, fmt.Errorf("model is required because the live catalog has no default; valid: [%s]", strings.Join(modelIDs(catalog), ", "))
		}
	}

	if choice.Effort != "" && !modelSupportsEffort(*selected, choice.Effort) {
		return ModelChoice{}, fmt.Errorf("effort %q not supported by model %q; valid: [%s]",
			choice.Effort, selected.ID, strings.Join(modelEfforts(*selected), ", "))
	}
	if choice.ServiceTier != "" && !modelSupportsServiceTier(*selected, choice.ServiceTier) {
		return ModelChoice{}, fmt.Errorf("service tier %q not supported by model %q; valid: [%s]",
			choice.ServiceTier, selected.ID, strings.Join(modelServiceTiers(*selected), ", "))
	}

	choice.Model = selected.Model
	if choice.Model == "" {
		choice.Model = selected.ID
	}
	return choice, nil
}

func modelIDs(catalog []Model) []string {
	values := make([]string, 0, len(catalog))
	for _, model := range catalog {
		if model.ID != "" {
			values = append(values, model.ID)
		}
	}
	sort.Strings(values)
	return values
}

func modelSupportsEffort(model Model, effort string) bool {
	for _, option := range model.SupportedReasoningEfforts {
		if option.ReasoningEffort == effort {
			return true
		}
	}
	return false
}

func modelEfforts(model Model) []string {
	values := make([]string, 0, len(model.SupportedReasoningEfforts))
	for _, option := range model.SupportedReasoningEfforts {
		if option.ReasoningEffort != "" {
			values = append(values, option.ReasoningEffort)
		}
	}
	sort.Strings(values)
	return values
}

func modelSupportsServiceTier(model Model, tier string) bool {
	for _, option := range model.ServiceTiers {
		if option.ID == tier {
			return true
		}
	}
	return false
}

func modelServiceTiers(model Model) []string {
	values := make([]string, 0, len(model.ServiceTiers))
	for _, option := range model.ServiceTiers {
		if option.ID != "" {
			values = append(values, option.ID)
		}
	}
	sort.Strings(values)
	return values
}
