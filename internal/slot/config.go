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
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadConfig reads Config from the environment and validates the origins and Google credentials.
func LoadConfig() (Config, error) {
	c := Config{DataDir: env("DATA_DIR", "data"), PublicAddr: env("PUBLIC_ADDR", ":8080"), AdminAddr: env("ADMIN_ADDR", "127.0.0.1:8081"), PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"), AdminURL: strings.TrimRight(env("ADMIN_URL", "http://localhost:8081"), "/"), GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"), GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"), RegistrationCode: os.Getenv("REGISTRATION_CODE"), RegistrationOpen: env("REGISTRATION_OPEN", "true") == "true"}
	for _, raw := range []string{c.PublicURL, c.AdminURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("PUBLIC_URL and ADMIN_URL must be absolute http(s) origins without a path")
		}
	}
	if c.PublicURL == c.AdminURL {
		return c, errors.New("public and admin origins must differ")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return c, errors.New("set both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET")
	}
	return c, nil
}
