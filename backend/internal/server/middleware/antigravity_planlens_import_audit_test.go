package middleware

import "testing"

func TestAntigravityPlanlensAuditOmitsCredentials(t *testing.T) {
	for _, route := range []string{"POST /api/v1/admin/accounts/antigravity-planlens-import/prepare", "POST /api/v1/admin/accounts/antigravity-planlens-import/commit"} {
		if _, ok := auditBodyOmittedRoutes[route]; !ok {
			t.Fatal("OAuth payload could enter audit log", route)
		}
	}
}
