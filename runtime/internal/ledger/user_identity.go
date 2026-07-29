package ledger

import "fmt"

// LocalUserCreatorID returns the stable, non-secret OS identity used to group
// Sessions records owned by the signed-in user. The platform-specific value is
// deliberately an opaque provenance key; it is not an authorization token.
func LocalUserCreatorID() (string, error) {
	id, err := platformLocalUserCreatorID()
	if err != nil {
		return "", fmt.Errorf("resolve local user creator identity: %w", err)
	}
	if err := ValidateCreator(CreatorUser, id); err != nil {
		return "", fmt.Errorf("resolve local user creator identity: %w", err)
	}
	return id, nil
}
