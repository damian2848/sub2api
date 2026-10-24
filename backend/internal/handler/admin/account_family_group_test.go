package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountFamilyGroupFilterReachesListRefreshAndExport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/accounts", "/accounts/upstream-billing-rates", "/accounts/data"} {
		t.Run(path, func(t *testing.T) {
			svc := &stubAdminService{}
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.GET("/accounts", h.List)
			router.GET("/accounts/upstream-billing-rates", h.GetUpstreamBillingRates)
			router.GET("/accounts/data", h.ExportData)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?family_group=Family%20A", nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "Family A", svc.lastListAccounts.familyGroup)
		})
	}
	filters := toServiceBulkUpdateAccountFilters(&BulkUpdateAccountFilters{FamilyGroup: service.AccountFamilyGroupUnset})
	require.Equal(t, service.AccountFamilyGroupUnset, filters.FamilyGroup)
}
