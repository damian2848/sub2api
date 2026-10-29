package service

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const AccountFamilyGroupKey = "family_group"
const AccountFamilyGroupUnset = "__unset__"

// Family groups describe account ownership independently of scheduling groups.
type AccountFamilyGroup struct {
	Name         string `json:"name"`
	AccountCount int    `json:"account_count"`
}

type accountFamilyGroupFilterKey struct{}

func WithAccountFamilyGroupFilter(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, accountFamilyGroupFilterKey{}, strings.TrimSpace(name))
}

func AccountFamilyGroupFilter(ctx context.Context) string {
	name, _ := ctx.Value(accountFamilyGroupFilterKey{}).(string)
	return name
}

// NormalizeAccountFamilyGroupExtra preserves absent keys and uses an empty
// string to remove membership through the existing JSONB merge update path.
func NormalizeAccountFamilyGroupExtra(extra map[string]any) error {
	raw, exists := extra[AccountFamilyGroupKey]
	if !exists {
		return nil
	}
	if raw == nil {
		extra[AccountFamilyGroupKey] = ""
		return nil
	}
	name, ok := raw.(string)
	if !ok {
		return infraerrors.BadRequest("INVALID_FAMILY_GROUP", "family group must be a string")
	}
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 || name == AccountFamilyGroupUnset || strings.ContainsFunc(name, unicode.IsControl) {
		return infraerrors.BadRequest("INVALID_FAMILY_GROUP", "family group must be at most 100 characters, without control characters or the reserved name __unset__")
	}
	extra[AccountFamilyGroupKey] = name
	return nil
}

type AccountFamilyGroupReader interface {
	ListAccountFamilyGroups(ctx context.Context) ([]AccountFamilyGroup, error)
}

func (s *adminServiceImpl) ListAccountFamilyGroups(ctx context.Context) ([]AccountFamilyGroup, error) {
	reader, ok := s.accountRepo.(AccountFamilyGroupReader)
	if !ok {
		return nil, infraerrors.InternalServer("FAMILY_GROUPS_UNAVAILABLE", "family group listing is unavailable")
	}
	return reader.ListAccountFamilyGroups(ctx)
}
