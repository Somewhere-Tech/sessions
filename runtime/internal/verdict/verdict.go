// Package verdict stores explicit producer verdicts. It deliberately does not
// inspect terminal output or provider prose: a verdict exists only after a
// producer emits one through the protocol in this package.
package verdict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const SchemaVersion = 1

var (
	ErrInvalid  = errors.New("invalid verdict")
	ErrNotFound = errors.New("verdict not found")
)

type Finding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	File     string `json:"file,omitempty"`
	Line     *int   `json:"line,omitempty"`
}

type Document struct {
	SchemaVersion int            `json:"schemaVersion"`
	Verdict       string         `json:"verdict"`
	Findings      []Finding      `json:"findings,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
}

type Record struct {
	SchemaVersion int            `json:"schemaVersion"`
	Verdict       string         `json:"verdict"`
	Findings      []Finding      `json:"findings,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
	Seq           uint64         `json:"seq"`
	EmittedAt     string         `json:"emitted_at"`
}

type Summary struct {
	Verdict      string `json:"verdict"`
	Seq          uint64 `json:"seq"`
	EmittedAt    string `json:"emitted_at"`
	FindingCount int    `json:"finding_count"`
}

func (r Record) Summary() Summary {
	return Summary{
		Verdict: r.Verdict, Seq: r.Seq, EmittedAt: r.EmittedAt,
		FindingCount: len(r.Findings),
	}
}

// Decode accepts exactly one strict JSON object. Unknown fields, trailing JSON,
// null where an object/array is required, and incomplete findings are rejected.
func Decode(reader io.Reader) (Document, error) {
	encoded, err := io.ReadAll(reader)
	if err != nil {
		return Document{}, fmt.Errorf("%w: read JSON: %v", ErrInvalid, err)
	}
	if len(bytes.TrimSpace(encoded)) == 0 {
		return Document{}, fmt.Errorf("%w: JSON body is required", ErrInvalid)
	}
	if err := rejectDuplicateKeys(encoded); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	var fields map[string]json.RawMessage
	if err := decodeStrict(encoded, &fields); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if fields == nil {
		return Document{}, fmt.Errorf("%w: verdict must be a JSON object", ErrInvalid)
	}
	for _, name := range []string{"schemaVersion", "verdict"} {
		if _, present := fields[name]; !present {
			return Document{}, fmt.Errorf("%w: %s is required", ErrInvalid, name)
		}
	}
	if raw, present := fields["findings"]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Document{}, fmt.Errorf("%w: findings must be an array", ErrInvalid)
	} else if present {
		var rawFindings []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawFindings); err == nil {
			for index, finding := range rawFindings {
				for _, name := range []string{"detail", "file", "line"} {
					if value, exists := finding[name]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						return Document{}, fmt.Errorf("%w: findings[%d].%s must not be null", ErrInvalid, index, name)
					}
				}
			}
		}
	}
	if raw, present := fields["meta"]; present {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return Document{}, fmt.Errorf("%w: meta must be an object", ErrInvalid)
		}
	}

	var document Document
	if err := decodeStrict(encoded, &document); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := Validate(document); err != nil {
		return Document{}, err
	}
	return document, nil
}

func rejectDuplicateKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key must be a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected delimiter %q", delimiter)
		}
	}
	return walk()
}

func Validate(document Document) error {
	if document.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schemaVersion must be %d", ErrInvalid, SchemaVersion)
	}
	if strings.TrimSpace(document.Verdict) == "" {
		return fmt.Errorf("%w: verdict must be a non-empty string", ErrInvalid)
	}
	for index, finding := range document.Findings {
		if strings.TrimSpace(finding.Severity) == "" {
			return fmt.Errorf("%w: findings[%d].severity must be a non-empty string", ErrInvalid, index)
		}
		if strings.TrimSpace(finding.Title) == "" {
			return fmt.Errorf("%w: findings[%d].title must be a non-empty string", ErrInvalid, index)
		}
		if finding.Line != nil && *finding.Line < 1 {
			return fmt.Errorf("%w: findings[%d].line must be positive", ErrInvalid, index)
		}
	}
	return nil
}

func decodeStrict(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}
