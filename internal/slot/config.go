package slot

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// Config is the server configuration, read from environment variables (see README).
type Config struct {
	DataDir, PublicAddr, AdminAddr, PublicURL, AdminURL  string
	GoogleClientID, GoogleClientSecret, RegistrationCode string
	RegistrationOpen                                     bool
	// HostAdminSeparately serves the admin on its own listener and origin (AdminAddr,
	// AdminURL). Otherwise it is served under /admin on the public listener.
	HostAdminSeparately bool
	// SingleHost is the URL name of the only host. Their page is served at the public
	// root, and registration closes once their account exists.
	SingleHost string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadConfig reads Config from the environment and validates the origins and Google credentials.
func LoadConfig() (Config, error) {
	c := Config{DataDir: env("DATA_DIR", "data"), PublicAddr: env("PUBLIC_ADDR", ":8080"), AdminAddr: env("ADMIN_ADDR", "127.0.0.1:8081"), PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"), AdminURL: strings.TrimRight(env("ADMIN_URL", "http://localhost:8081"), "/"), GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"), GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"), RegistrationCode: os.Getenv("REGISTRATION_CODE"), RegistrationOpen: env("REGISTRATION_OPEN", "true") == "true", HostAdminSeparately: env("HOST_ADMIN_SEPARATELY", "false") == "true", SingleHost: strings.ToLower(strings.TrimSpace(os.Getenv("SINGLE_HOST_URL_NAME")))}
	if os.Getenv("SINGLE_HOST") != "" {
		return c, errors.New("SINGLE_HOST was renamed to SINGLE_HOST_URL_NAME")
	}
	if !c.HostAdminSeparately {
		// ADMIN_URL decides the OAuth callback, so ignoring it would silently break Google.
		if os.Getenv("ADMIN_URL") != "" {
			return c, errors.New("ADMIN_URL applies only with HOST_ADMIN_SEPARATELY=true; the admin is otherwise served at PUBLIC_URL/admin")
		}
	}
	for _, raw := range []string{c.PublicURL, c.AdminURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("PUBLIC_URL and ADMIN_URL must be absolute http(s) origins without a path")
		}
	}
	if c.HostAdminSeparately && c.PublicURL == c.AdminURL {
		return c, errors.New("with HOST_ADMIN_SEPARATELY=true, PUBLIC_URL and ADMIN_URL must differ")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return c, errors.New("set both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET")
	}
	if c.SingleHost != "" && !slugPattern.MatchString(c.SingleHost) {
		return c, errors.New("SINGLE_HOST_URL_NAME must be a booking URL name: 1–40 lowercase letters, digits, or hyphens")
	}
	// Otherwise whoever reaches the admin first becomes the only host.
	if c.SingleHost != "" && c.RegistrationCode == "" {
		return c, errors.New("SINGLE_HOST_URL_NAME requires REGISTRATION_CODE")
	}
	return c, nil
}
