package domain

import "testing"

// TestRouteBreakerKeySeparatesAccounts 守护熔断与折叠口径按账号分开。
//
// 一个账号被限流不应该把整条渠道熔断，而同一账号的重复失败仍要归到同一个窗口里折叠；
// 单账号时键不加后缀，与账号池引入之前逐字一致。
func TestRouteBreakerKeySeparatesAccounts(t *testing.T) {
	tests := []struct {
		name       string
		upstreamID string
		accountRef string
		want       string
	}{
		{
			name:       "单账号不加后缀",
			upstreamID: "relay a.example.com",
			want:       "relay a.example.com",
		},
		{
			name:       "多账号按账号分开",
			upstreamID: "relay a.example.com",
			accountRef: "#2",
			want:       "relay a.example.com #2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := Route{UpstreamID: tt.upstreamID, AccountRef: tt.accountRef}
			if got := route.BreakerKey(); got != tt.want {
				t.Errorf("BreakerKey() = %q，期望 %q", got, tt.want)
			}
		})
	}
}
