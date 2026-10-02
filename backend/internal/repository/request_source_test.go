//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestSourcePersistenceDefaultsToBusiness(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", service.RequestSourceBusiness},
		{"business", service.RequestSourceBusiness},
		{"probe", service.RequestSourceProbe},
		{"PROBE", service.RequestSourceBusiness},
		{" probe ", service.RequestSourceBusiness},
		{"untrusted", service.RequestSourceBusiness},
	} {
		t.Run(tc.input, func(t *testing.T) {
			usage := prepareUsageLogInsert(&service.UsageLog{Source: tc.input})
			require.Equal(t, tc.want, usage.args[len(usage.args)-5])
			require.Equal(t, "text", usageLogInsertArgTypes[len(usage.args)-5])
			ops := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{Source: tc.input})
			require.Equal(t, tc.want, ops[len(ops)-1])
		})
	}
}
