package util

import "testing"

func TestWithImportMargin(t *testing.T) {
	tests := []struct {
		name          string
		size          int64
		marginPercent int
		want          int64
	}{
		{name: "zero margin is a no-op", size: 333688832, marginPercent: 0, want: 333688832},
		{name: "negative margin is a no-op", size: 333688832, marginPercent: -10, want: 333688832},
		{name: "typical boot ISO size", size: 333688832, marginPercent: 30, want: 433795481},
		{name: "1 GiB", size: 1 << 30, marginPercent: 30, want: 1395864371},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WithImportMargin(tt.size, tt.marginPercent); got != tt.want {
				t.Errorf("WithImportMargin(%d, %d) = %d, want %d", tt.size, tt.marginPercent, got, tt.want)
			}
		})
	}
}
