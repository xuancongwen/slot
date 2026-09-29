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
		{name: "defaults", check: func(c Config) bool { return !c.HostAdminSeparately && c.SingleHost == "" && c.AnalyticsScript == "" }},
		{name: "analytics", env: map[string]string{"ANALYTICS_SCRIPT_URL": "https://stats.example.com/script.js", "ANALYTICS_SCRIPT_ATTRS": "data-website-id=abc  data-auto-track=false"}, check: func(c Config) bool {
			return len(c.AnalyticsAttrs) == 2 && c.AnalyticsAttrs[0] == [2]string{"data-website-id", "abc"} && c.AnalyticsAttrs[1] == [2]string{"data-auto-track", "false"}
		}},
		{name: "analytics script not a URL", env: map[string]string{"ANALYTICS_SCRIPT_URL": "/script.js"}, wantErr: "ANALYTICS_SCRIPT_URL"},
		{name: "analytics attributes without a script", env: map[string]string{"ANALYTICS_SCRIPT_ATTRS": "data-website-id=abc"}, wantErr: "requires ANALYTICS_SCRIPT_URL"},
		{name: "analytics attribute not data-*", env: map[string]string{"ANALYTICS_SCRIPT_URL": "https://stats.example.com/script.js", "ANALYTICS_SCRIPT_ATTRS": "onload=alert(1)"}, wantErr: "data-name=value"},
		{name: "single host with a code", env: map[string]string{"SINGLE_HOST_URL_NAME": " Sam ", "REGISTRATION_CODE": "secret"}, check: func(c Config) bool { return c.SingleHost == "sam" }},
		{name: "single host without a code", env: map[string]string{"SINGLE_HOST_URL_NAME": "sam"}, wantErr: "REGISTRATION_CODE"},
		{name: "single host not a URL name", env: map[string]string{"SINGLE_HOST_URL_NAME": "sam/wen", "REGISTRATION_CODE": "secret"}, wantErr: "SINGLE_HOST_URL_NAME"},
		{name: "old single host name", env: map[string]string{"SINGLE_HOST": "sam"}, wantErr: "renamed to SINGLE_HOST_URL_NAME"},
		{name: "admin URL without separate hosting", env: map[string]string{"ADMIN_URL": "https://admin.example.com"}, wantErr: "HOST_ADMIN_SEPARATELY"},
		{name: "separate admin", env: map[string]string{"HOST_ADMIN_SEPARATELY": "true", "ADMIN_URL": "https://admin.example.com"}, check: func(c Config) bool { return c.HostAdminSeparately && c.AdminURL == "https://admin.example.com" }},
		{name: "mail", env: map[string]string{"SMTP_HOST": "smtp.gmail.com", "SMTP_USERNAME": "me@gmail.com", "SMTP_PASSWORD": "app-password", "MAIL_FROM": "Slot <me@gmail.com>"}, check: func(c Config) bool { return c.SMTPHost == "smtp.gmail.com" && c.SMTPPort == "587" }},
		{name: "mail settings without a server", env: map[string]string{"MAIL_FROM": "me@gmail.com"}, wantErr: "require SMTP_HOST"},
		{name: "mail without a sender", env: map[string]string{"SMTP_HOST": "smtp.gmail.com"}, wantErr: "MAIL_FROM"},
		{name: "mail username without password", env: map[string]string{"SMTP_HOST": "smtp.gmail.com", "SMTP_USERNAME": "me@gmail.com", "MAIL_FROM": "me@gmail.com"}, wantErr: "SMTP_PASSWORD"},
		{name: "mail port not a number", env: map[string]string{"SMTP_HOST": "smtp.gmail.com", "SMTP_PORT": "smtps", "MAIL_FROM": "me@gmail.com"}, wantErr: "SMTP_PORT"},
		{name: "separate admin on the public origin", env: map[string]string{"HOST_ADMIN_SEPARATELY": "true", "PUBLIC_URL": "https://book.example.com", "ADMIN_URL": "https://book.example.com"}, wantErr: "must differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"SINGLE_HOST", "SINGLE_HOST_URL_NAME", "REGISTRATION_CODE", "HOST_ADMIN_SEPARATELY", "PUBLIC_URL", "ADMIN_URL", "ANALYTICS_SCRIPT_URL", "ANALYTICS_SCRIPT_ATTRS", "SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD", "MAIL_FROM"} {
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
