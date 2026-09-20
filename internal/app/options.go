package app

import (
	"context"
	"fmt"
	"time"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/config"
	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
)

// GatewayOptions maps the resolved config into gateway.Options and TLS config,
// merging in the given hooks and custom routes. It is the single place config
// becomes runtime options, shared by the library App and the `dcms` binary (both
// via Serve), so the two never diverge.
func (s *Server) GatewayOptions(ctx context.Context, hooks *gateway.HookRegistry, routes []gateway.CustomRoute) (gateway.Options, engine.TLSConfig, error) {
	cfg := s.cfg
	var zero gateway.Options

	// Strict response validation: the config's explicit setting wins, else the
	// mode default (on in dev, off in serve).
	validateResponses := s.dev
	if cfg.Server.ValidateResponses != nil {
		validateResponses = *cfg.Server.ValidateResponses
	}

	bs, err := blob.New(blob.Config{
		Driver: cfg.Media.Driver, Dir: cfg.Media.Dir, Endpoint: cfg.Media.Endpoint,
		Region: cfg.Media.Region, Bucket: cfg.Media.Bucket, AccessKey: cfg.Media.AccessKey,
		SecretKey: cfg.Media.SecretKey, UseSSL: cfg.Media.UseSSL,
		ForcePathStyle: cfg.Media.ForcePathStyle, PublicBaseURL: cfg.Media.PublicBaseURL,
	})
	if err != nil {
		return zero, engine.TLSConfig{}, err
	}

	// Rate limiting defaults ON; an explicit enabled:false leaves it nil (off).
	var rateLimit *gateway.RateLimitOptions
	if cfg.Server.RateLimit.Enabled == nil || *cfg.Server.RateLimit.Enabled {
		rateLimit = &gateway.RateLimitOptions{
			APIPerMinute: cfg.Server.RateLimit.APIPerMinute, APIBurst: cfg.Server.RateLimit.APIBurst,
			AuthPerMinute: cfg.Server.RateLimit.AuthPerMinute, AuthBurst: cfg.Server.RateLimit.AuthBurst,
			AnonWritePerMinute: cfg.Server.RateLimit.AnonWritePerMinute, AnonWriteBurst: cfg.Server.RateLimit.AnonWriteBurst,
		}
	}

	var cors *gateway.CORSOptions
	if len(cfg.Server.CORS.AllowedOrigins) > 0 {
		cors = &gateway.CORSOptions{
			AllowedOrigins: cfg.Server.CORS.AllowedOrigins, AllowedMethods: cfg.Server.CORS.AllowedMethods,
			AllowedHeaders: cfg.Server.CORS.AllowedHeaders, ExposedHeaders: cfg.Server.CORS.ExposedHeaders,
			AllowCredentials: cfg.Server.CORS.AllowCredentials, MaxAgeSeconds: cfg.Server.CORS.MaxAgeSeconds,
		}
	}

	var idempotency *gateway.IdempotencyOptions
	if cfg.Server.Idempotency.Enabled == nil || *cfg.Server.Idempotency.Enabled {
		idempotency = &gateway.IdempotencyOptions{TTL: time.Duration(cfg.Server.Idempotency.TTLHours) * time.Hour}
	}

	adminRoles := cfg.Auth.AdminRoles
	if len(adminRoles) == 0 {
		adminRoles = []string{"admin"}
	}
	var registration *gateway.RegistrationOptions
	if cfg.Auth.Registration.Enabled {
		for _, role := range cfg.Auth.Registration.DefaultRoles {
			if !s.def.HasRole(role) {
				return zero, engine.TLSConfig{}, fmt.Errorf("registration default_role %q is not a declared role", role)
			}
			for _, ar := range adminRoles {
				if role == ar {
					return zero, engine.TLSConfig{}, fmt.Errorf("registration default_role %q may not be an admin role", role)
				}
			}
		}
		registration = &gateway.RegistrationOptions{DefaultRoles: cfg.Auth.Registration.DefaultRoles}
	}

	var otpLogin *gateway.OTPLoginOptions
	if cfg.Auth.OTP.Enabled {
		otpLogin = &gateway.OTPLoginOptions{
			TTL:         time.Duration(cfg.Auth.OTP.TTLMinutes) * time.Minute,
			MaxAttempts: cfg.Auth.OTP.MaxAttempts,
		}
	}

	// Mailer: SMTP when a host is set, else the gateway's dev-log notifier. A set
	// host is pre-flighted so a broken mail path surfaces at startup (non-fatal).
	var notifier gateway.Notifier
	if cfg.Auth.SMTP.Host != "" {
		notifier = gateway.NewSMTPNotifier(cfg.Auth.SMTP.Host, cfg.Auth.SMTP.Port,
			cfg.Auth.SMTP.From, cfg.Auth.SMTP.Username, cfg.Auth.SMTP.Password)
		if v, ok := notifier.(gateway.ConnectionVerifier); ok {
			vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := v.VerifyConnection(vctx); err != nil {
				s.logger.Warn("SMTP pre-flight failed — account emails will not send until resolved",
					"host", cfg.Auth.SMTP.Host, "port", cfg.Auth.SMTP.Port, "err", err)
			} else {
				s.logger.Info("SMTP connection verified", "host", cfg.Auth.SMTP.Host)
			}
			cancel()
		}
	}

	var webhooks *gateway.WebhookOptions
	if len(cfg.Events.Webhooks) > 0 {
		eps := make([]gateway.WebhookEndpoint, 0, len(cfg.Events.Webhooks))
		for _, wh := range cfg.Events.Webhooks {
			if wh.Name == "" || wh.URL == "" {
				return zero, engine.TLSConfig{}, fmt.Errorf("webhook: name and url are required")
			}
			if wh.Secret == "" {
				return zero, engine.TLSConfig{}, fmt.Errorf("webhook %q: secret is empty (set secret_env)", wh.Name)
			}
			eps = append(eps, gateway.WebhookEndpoint{
				Name: wh.Name, URL: wh.URL, Secret: wh.Secret,
				Events: wh.Events, Collections: wh.Collections, MaxAttempts: wh.MaxAttempts,
			})
		}
		webhooks = &gateway.WebhookOptions{Endpoints: eps, PollInterval: time.Duration(cfg.Events.WebhookPollSeconds) * time.Second}
	}

	// Authenticator: built-in sessions, or a proxy_header trust (ADR-0020). API
	// tokens layer over whichever is selected (issue #8).
	var authenticator gateway.Authenticator
	switch cfg.Auth.Provider {
	case "", "session":
		authenticator = gateway.NewSessionAuthenticator(s.db)
	case "proxy_header":
		if cfg.Auth.ProxyHeader.UserHeader == "" {
			return zero, engine.TLSConfig{}, fmt.Errorf("auth.provider proxy_header requires auth.proxy_header.user_header")
		}
		authenticator = gateway.NewProxyHeaderAuthenticator(
			cfg.Auth.ProxyHeader.UserHeader, cfg.Auth.ProxyHeader.RolesHeader, cfg.Auth.ProxyHeader.RolesSeparator)
		s.logger.Warn("auth provider is proxy_header — DCMS trusts the identity header verbatim; your proxy MUST strip it from inbound requests",
			"user_header", cfg.Auth.ProxyHeader.UserHeader)
	default:
		return zero, engine.TLSConfig{}, fmt.Errorf("auth.provider %q is not supported (want session or proxy_header)", cfg.Auth.Provider)
	}
	authenticator = gateway.WithAPITokens(s.db, authenticator)

	var mediaQuota *gateway.MediaQuotaOptions
	if q := cfg.Media.Quotas; q.Default != "" || len(q.Roles) > 0 {
		if q.Default == "" {
			return zero, engine.TLSConfig{}, fmt.Errorf("media.quotas.default is required when media.quotas is set")
		}
		def, err := config.ParseByteSize(q.Default)
		if err != nil {
			return zero, engine.TLSConfig{}, fmt.Errorf("media.quotas.default: %w", err)
		}
		mediaQuota = &gateway.MediaQuotaOptions{Default: def, Roles: map[string]int64{}}
		for role, sz := range q.Roles {
			n, err := config.ParseByteSize(sz)
			if err != nil {
				return zero, engine.TLSConfig{}, fmt.Errorf("media.quotas.roles.%s: %w", role, err)
			}
			mediaQuota.Roles[role] = n
		}
	}

	opts := gateway.Options{
		ValidateResponses:      validateResponses,
		Blob:                   bs,
		MaxUploadBytes:         cfg.Media.MaxUploadBytes,
		AllowedContentTypes:    cfg.Media.AllowedContentTypes,
		MediaQuota:             mediaQuota,
		PreviewToken:           cfg.Content.PreviewToken,
		Introspection:          cfg.Server.Introspection,
		Authenticator:          authenticator,
		MaxBodyBytes:           cfg.Server.MaxBodyBytes,
		RequestTimeout:         time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		RateLimit:              rateLimit,
		Idempotency:            idempotency,
		TrustProxy:             cfg.Server.TrustProxy,
		CORS:                   cors,
		AdminRoles:             adminRoles,
		Registration:           registration,
		PasswordMinLength:      cfg.Auth.Password.MinLength,
		Notifier:               notifier,
		ResetLinkBase:          cfg.Auth.Reset.LinkBase,
		ResetLinkBases:         cfg.Auth.Reset.LinkBases,
		ResetTokenTTL:          time.Duration(cfg.Auth.Reset.TTLMinutes) * time.Minute,
		OTPLogin:               otpLogin,
		MailMaxPerDay:          cfg.Auth.Mail.MaxPerDay,
		MailPerRecipientPerDay: cfg.Auth.Mail.PerRecipientPerDay,
		Admin:                  &gateway.AdminOptions{Enabled: cfg.Admin.Enabled == nil || *cfg.Admin.Enabled},
		Webhooks:               webhooks,
		Hooks:                  hooks,
		Routes:                 routes,
	}
	tlsCfg := engine.TLSConfig{CertFile: cfg.Server.TLS.CertFile, KeyFile: cfg.Server.TLS.KeyFile}
	return opts, tlsCfg, nil
}
