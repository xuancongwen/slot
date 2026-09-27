package slot

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(Config) bool
	}{
		{name: "defaults", check: func(c Config) bool { return !c.HostAdminSeparately && c.SingleHost == "" }},
		{name: "single host with a code", env: map[string]string{"SINGLE_HOST_URL_NAME": " Sam ", "REGISTRATION_CODE": "secret"}, check: func(c Config) bool { return c.SingleHost == "sam" }},
		{name: "single host without a code", env: map[string]string{"SINGLE_HOST_URL_NAME": "sam"}, wantErr: "REGISTRATION_CODE"},
		{name: "single host not a URL name", env: map[string]string{"SINGLE_HOST_URL_NAME": "sam/wen", "REGISTRATION_CODE": "secret"}, wantErr: "SINGLE_HOST_URL_NAME"},
		{name: "old single host name", env: map[string]string{"SINGLE_HOST": "sam"}, wantErr: "renamed to SINGLE_HOST_URL_NAME"},
		{name: "admin URL without separate hosting", env: map[string]string{"ADMIN_URL": "https://admin.example.com"}, wantErr: "HOST_ADMIN_SEPARATELY"},
		{name: "separate admin", env: map[string]string{"HOST_ADMIN_SEPARATELY": "true", "ADMIN_URL": "https://admin.example.com"}, check: func(c Config) bool { return c.HostAdminSeparately && c.AdminURL == "https://admin.example.com" }},
		{name: "separate admin on the public origin", env: map[string]string{"HOST_ADMIN_SEPARATELY": "true", "PUBLIC_URL": "https://book.example.com", "ADMIN_URL": "https://book.example.com"}, wantErr: "must differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"SINGLE_HOST", "SINGLE_HOST_URL_NAME", "REGISTRATION_CODE", "HOST_ADMIN_SEPARATELY", "PUBLIC_URL", "ADMIN_URL"} {
				t.Setenv(key, tc.env[key])
			}
			c, e := LoadConfig()
			if tc.wantErr != "" {
				if e == nil || !strings.Contains(e.Error(), tc.wantErr) {
					t.Fatalf("error %v, want mention of %q", e, tc.wantErr)
				}
				return
			}
			if e != nil || !tc.check(c) {
				t.Fatalf("config %+v, error %v", c, e)
			}
		})
	}
}
