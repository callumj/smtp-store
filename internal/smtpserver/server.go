package smtpserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"

	"smtp-store/internal/config"
	"smtp-store/internal/storage"
)

// New constructs an SMTP server configured for local authenticated capture.
func New(cfg *config.Config, store *storage.Store, logger *slog.Logger) (*smtp.Server, error) {
	backend := &backend{
		users:   cfg.UserMap(),
		store:   store,
		logger:  logger,
		verbose: cfg.VerboseLogs,
	}

	srv := smtp.NewServer(backend)
	srv.Addr = cfg.ListenAddr
	srv.Domain = cfg.Hostname
	srv.ReadTimeout = 30 * time.Second
	srv.WriteTimeout = 30 * time.Second
	srv.MaxMessageBytes = 100 << 20 // 100 MiB
	srv.MaxRecipients = 32
	srv.AllowInsecureAuth = true

	if cfg.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS cert/key: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	return srv, nil
}

type backend struct {
	users      map[string]string
	store      *storage.Store
	logger     *slog.Logger
	verbose    bool
	nextConnID atomic.Uint64
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	connID := b.nextConnID.Add(1)
	remote := remoteAddress(c)
	_, tlsOn := c.TLSConnectionState()
	sess := &session{
		backend:   b,
		connID:    connID,
		remote:    remote,
		tlsActive: tlsOn,
	}

	sess.vlog("connection opened")
	return sess, nil
}

type session struct {
	backend       *backend
	connID        uint64
	remote        string
	tlsActive     bool
	authenticated bool
	username      string
	from          string
	rcpts         []string
}

func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	s.vlog("auth mechanism requested", "mechanism", mech)
	if !strings.EqualFold(mech, sasl.Plain) {
		s.vlog("auth failed", "mechanism", mech, "reason", "unknown-mechanism")
		return nil, smtp.ErrAuthUnknownMechanism
	}

	return sasl.NewPlainServer(func(identity, username, password string) error {
		normalizedUser := strings.ToLower(strings.TrimSpace(username))
		storedPassword, ok := s.backend.users[normalizedUser]
		if !ok || storedPassword != password {
			s.vlog("auth failed", "mechanism", mech, "username", normalizedUser, "reason", "bad-credentials")
			return smtp.ErrAuthFailed
		}

		s.authenticated = true
		s.username = normalizedUser
		s.vlog("auth succeeded", "mechanism", mech, "username", normalizedUser)
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.vlog("mail from received", "from", from, "authenticated", s.authenticated)
	if !s.authenticated {
		s.vlog("mail from rejected", "reason", "authentication-required")
		return &smtp.SMTPError{
			Code:         530,
			EnhancedCode: smtp.EnhancedCode{5, 7, 0},
			Message:      "authentication required",
		}
	}

	s.from = strings.TrimSpace(from)
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.vlog("recipient received", "recipient", to)
	to = strings.TrimSpace(to)
	if to == "" {
		s.vlog("recipient rejected", "reason", "empty-recipient")
		return &smtp.SMTPError{Code: 501, Message: "empty recipient"}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	s.vlog("message data received", "authenticated", s.authenticated, "recipient_count", len(s.rcpts))
	if !s.authenticated {
		s.vlog("message rejected", "reason", "authentication-required")
		return &smtp.SMTPError{
			Code:         530,
			EnhancedCode: smtp.EnhancedCode{5, 7, 0},
			Message:      "authentication required",
		}
	}
	if len(s.rcpts) == 0 {
		s.vlog("message rejected", "reason", "no-recipients")
		return &smtp.SMTPError{Code: 554, Message: "no recipients"}
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		s.vlog("message rejected", "reason", "read-failed", "error", err)
		return &smtp.SMTPError{Code: 451, Message: "failed to read message body"}
	}
	s.vlog("message payload read", "bytes", len(raw))

	result, err := s.backend.store.ProcessAndStore(raw, time.Now())
	if err != nil {
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) {
			s.vlog("message rejected", "reason", "smtp-error", "error", smtpErr)
			return smtpErr
		}
		s.backend.logger.Error("failed to store message", "error", err, "from", s.from, "auth_user", s.username)
		return &smtp.SMTPError{Code: 554, Message: "failed to process message"}
	}

	s.backend.logger.Info(
		"message stored",
		"from", s.from,
		"auth_user", s.username,
		"to", result.Recipient,
		"body_path", result.BodyPath,
		"attachments", len(result.AttachmentPaths),
	)

	return nil
}

func (s *session) Reset() {
	s.vlog("session reset")
	s.from = ""
	s.rcpts = s.rcpts[:0]
}

func (s *session) Logout() error {
	s.vlog("connection closed")
	return nil
}

func (s *session) vlog(msg string, attrs ...any) {
	if !s.backend.verbose {
		return
	}

	base := []any{
		"conn_id", s.connID,
		"remote_addr", s.remote,
		"tls_active", s.tlsActive,
	}
	if s.username != "" {
		base = append(base, "auth_user", s.username)
	}
	s.backend.logger.Info(msg, append(base, attrs...)...)
}

func remoteAddress(c *smtp.Conn) string {
	if c == nil {
		return "unknown"
	}
	nc := c.Conn()
	if nc == nil {
		return "unknown"
	}
	addr := nc.RemoteAddr()
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
