package slot

import (
	"strings"
	"testing"
)

func TestSingleHostConfig(t *testing.T) {
	for _, tc := range []struct {
		name, host, code, wantErr string
	}{
		{"unset", "", "", ""},
		{"with a code", " Sam ", "secret", ""},
		{"without a code", "sam", "", "REGISTRATION_CODE"},
		{"not a URL name", "sam/wen", "secret", "SINGLE_HOST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SINGLE_HOST", tc.host)
			t.Setenv("REGISTRATION_CODE", tc.code)
			c, e := LoadConfig()
			if tc.wantErr == "" && (e != nil || c.SingleHost != strings.ToLower(strings.TrimSpace(tc.host))) {
				t.Fatalf("config %+v, error %v", c, e)
			}
			if tc.wantErr != "" && (e == nil || !strings.Contains(e.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want mention of %s", e, tc.wantErr)
			}
		})
	}
}
