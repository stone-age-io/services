// Package natsconn connects a service to NATS the same way every time: a user
// creds file, a client name, and reconnecting forever.
package natsconn

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

// Hooks are called on connection events, after they are logged. Either may
// be nil.
type Hooks struct {
	Disconnected func()
	Reconnected  func()
}

// Connect connects to url. creds may be empty only in tests.
func Connect(url, creds, name string, log *slog.Logger, h Hooks) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", "error", err)
			if h.Disconnected != nil {
				h.Disconnected()
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrlRedacted())
			if h.Reconnected != nil {
				h.Reconnected()
			}
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			log.Error("nats async error", "subject", subject, "error", err)
		}),
	}
	if creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
	}
	return nats.Connect(url, opts...)
}
