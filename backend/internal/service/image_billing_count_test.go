package service

import "testing"

func TestResolveBillableImageCount(t *testing.T) {
	tests := []struct {
		name      string
		reported  int
		requested int
		want      int
	}{
		{name: "实际返回优先", reported: 3, requested: 5, want: 3},
		{name: "实际缺失回退请求 n", reported: 0, requested: 3, want: 3},
		{name: "都没给按一张", reported: 0, requested: 0, want: 1},
		{name: "非法负数忽略", reported: -1, requested: 4, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBillableImageCount(tt.reported, tt.requested); got != tt.want {
				t.Fatalf("resolveBillableImageCount(%d, %d) = %d, want %d", tt.reported, tt.requested, got, tt.want)
			}
		})
	}
}
