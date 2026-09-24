package model

import "testing"

func TestFormatIdentityUsesStableFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		id       string
		fallback string
		want     string
	}{
		{name: "Owner", email: "owner@example.com", id: "42", want: "Owner <owner@example.com> #42"},
		{name: "unknown", id: "42", fallback: "用户", want: "用户 #42"},
		{id: "42", want: "#42"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatIdentity(tt.name, tt.email, tt.id, tt.fallback); got != tt.want {
				t.Fatalf("FormatIdentity() = %q, want %q", got, tt.want)
			}
		})
	}
}
