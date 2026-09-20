package chathub

import "testing"

func TestCheckMeteringErrorRecognizesImageQuotaVariants(t *testing.T) {
	tests := []struct {
		name string
		code string
		want error
	}{
		{name: "legacy token throttle", code: "ImageGenInsufficientTokensThrottled", want: ErrImageLimit},
		{name: "daily limit variant", code: "ImageGenerationDailyLimitReached", want: ErrImageLimit},
		{name: "system capacity remains temporary", code: "ImageGenSystemCapacityThrottled", want: ErrMeteringThrottled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mi := []any{map[string]any{"meterError": tt.code, "hasAccess": false}}
			if got := checkMeteringError(mi); got != tt.want {
				t.Fatalf("checkMeteringError(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}
