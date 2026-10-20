package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// The omitted mode preserves the existing bulk API's replacement contract.
func validateBulkModelMappingUpdate(input *BulkUpdateAccountsInput) error {
	mode := input.ModelMappingUpdateMode
	if mode == "" {
		return nil
	}
	invalid := func() error {
		return infraerrors.BadRequest("INVALID_MODEL_MAPPING_UPDATE_MODE", "model_mapping_update_mode must be merge or replace with a string-to-string model_mapping object")
	}
	if mode != "merge" && mode != "replace" {
		return invalid()
	}
	raw, exists := input.Credentials["model_mapping"]
	if !exists {
		return invalid()
	}
	switch mapping := raw.(type) {
	case map[string]any:
		for from, value := range mapping {
			to, ok := value.(string)
			if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
				return invalid()
			}
		}
	case map[string]string:
		for from, to := range mapping {
			if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
				return invalid()
			}
		}
	default:
		return invalid()
	}
	return nil
}
