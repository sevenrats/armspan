package gormspan

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// notifyClient is a shared HTTP client with sensible timeouts.
var notifyClient = &http.Client{
	Timeout: 10 * time.Second,
}

// notifyFunc returns a function suitable for registration as an SQLite
// scalar function:
//
//	notify(message TEXT, url TEXT) → INTEGER
//
// It POSTs the message body as application/json to the given URL.
// Returns 1 on success, 0 on failure.
//
// When enabled is false the function is a no-op that always returns 1,
// useful for dev/test environments.
//
// If defaultURL is non-empty it is used when the SQL-level url argument
// is empty.
func notifyFunc(defaultURL string, enabled bool) func(message, url string) int64 {
	if !enabled {
		log.Info().Msg("gormspan: notify() SQL function registered (disabled / no-op)")
		return func(_, _ string) int64 { return 1 }
	}

	log.Info().Str("default_url", defaultURL).Msg("gormspan: notify() SQL function registered (enabled)")

	return func(message, url string) int64 {
		if url == "" {
			url = defaultURL
		}
		if url == "" {
			log.Warn().Msg("gormspan: notify() called with no URL")
			return 0
		}

		resp, err := notifyClient.Post(url, "application/json", bytes.NewBufferString(message))
		if err != nil {
			log.Error().Err(err).Str("url", url).Msg("gormspan: notify() HTTP request failed")
			return 0
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return 1
		}

		log.Warn().
			Int("status", resp.StatusCode).
			Str("url", url).
			Msg("gormspan: notify() received non-2xx response")
		return 0
	}
}

// SetNotifyClient allows replacing the default HTTP client, e.g. for
// testing or to inject custom transports / TLS configuration.
func SetNotifyClient(c *http.Client) {
	if c == nil {
		panic(fmt.Sprintf("gormspan: SetNotifyClient called with nil"))
	}
	notifyClient = c
}
