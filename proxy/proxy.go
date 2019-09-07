package proxy // import "github.com/pomerium/pomerium/proxy"

import (
	"crypto/tls"
	"fmt"
	"html/template"
	stdlog "log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/pomerium/pomerium/internal/config"
	"github.com/pomerium/pomerium/internal/cryptutil"
	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/middleware"
	"github.com/pomerium/pomerium/internal/sessions"
	"github.com/pomerium/pomerium/internal/telemetry/metrics"
	"github.com/pomerium/pomerium/internal/telemetry/trace"
	"github.com/pomerium/pomerium/internal/templates"
	"github.com/pomerium/pomerium/internal/tripper"
	"github.com/pomerium/pomerium/internal/urlutil"
	"github.com/pomerium/pomerium/proxy/clients"
)

const (
	// HeaderJWT is the header key containing JWT signed user details.
	HeaderJWT = "x-pomerium-jwt-assertion"
	// HeaderUserID is the header key containing the user's id.
	HeaderUserID = "x-pomerium-authenticated-user-id"
	// HeaderEmail is the header key containing the user's email.
	HeaderEmail = "x-pomerium-authenticated-user-email"
	// HeaderGroups is the header key containing the user's groups.
	HeaderGroups = "x-pomerium-authenticated-user-groups"
)

// ValidateOptions checks that proper configuration settings are set to create
// a proper Proxy instance
func ValidateOptions(o config.Options) error {
	if _, err := cryptutil.NewCipherFromBase64(o.SharedKey); err != nil {
		return fmt.Errorf("proxy: invalid 'SHARED_SECRET': %v", err)
	}
	if _, err := cryptutil.NewCipherFromBase64(o.CookieSecret); err != nil {
		return fmt.Errorf("proxy: invalid 'COOKIE_SECRET': %v", err)
	}
	if o.AuthenticateURL == nil {
		return fmt.Errorf("proxy: missing 'AUTHENTICATE_SERVICE_URL'")
	}
	if _, err := urlutil.ParseAndValidateURL(o.AuthenticateURL.String()); err != nil {
		return fmt.Errorf("proxy: invalid 'AUTHENTICATE_SERVICE_URL': %v", err)
	}
	if o.AuthorizeURL == nil {
		return fmt.Errorf("proxy: missing 'AUTHORIZE_SERVICE_URL'")
	}
	if _, err := urlutil.ParseAndValidateURL(o.AuthorizeURL.String()); err != nil {
		return fmt.Errorf("proxy: invalid 'AUTHORIZE_SERVICE_URL': %v", err)
	}
	if len(o.SigningKey) != 0 {
		if _, err := cryptutil.NewES256Signer(o.SigningKey, "localhost"); err != nil {
			return fmt.Errorf("proxy: invalid 'SIGNING_KEY': %v", err)
		}
	}
	return nil
}

// Proxy stores all the information associated with proxying a request.
type Proxy struct {
	// SharedKey used to mutually authenticate service communication
	SharedKey       string
	authenticateURL *url.URL
	authorizeURL    *url.URL

	AuthorizeClient clients.Authorizer

	cipher                 cryptutil.Cipher
	cookieName             string
	csrfStore              sessions.CSRFStore
	defaultUpstreamTimeout time.Duration
	redirectURL            *url.URL
	refreshCooldown        time.Duration
	routeConfigs           map[string]*routeConfig
	sessionStore           sessions.SessionStore
	signingKey             string
	templates              *template.Template
}

type routeConfig struct {
	mux    http.Handler
	policy config.Policy
}

// New takes a Proxy service from options and a validation function.
// Function returns an error if options fail to validate.
func New(opts config.Options) (*Proxy, error) {
	if err := ValidateOptions(opts); err != nil {
		return nil, err
	}
	cipher, err := cryptutil.NewCipherFromBase64(opts.CookieSecret)
	if err != nil {
		return nil, err
	}

	cookieStore, err := sessions.NewCookieStore(
		&sessions.CookieStoreOptions{
			Name:           opts.CookieName,
			CookieDomain:   opts.CookieDomain,
			CookieSecure:   opts.CookieSecure,
			CookieHTTPOnly: opts.CookieHTTPOnly,
			CookieExpire:   opts.CookieExpire,
			CookieCipher:   cipher,
		})

	if err != nil {
		return nil, err
	}
	p := &Proxy{
		SharedKey: opts.SharedKey,

		routeConfigs: make(map[string]*routeConfig),

		cipher:                 cipher,
		cookieName:             opts.CookieName,
		csrfStore:              cookieStore,
		defaultUpstreamTimeout: opts.DefaultUpstreamTimeout,
		redirectURL:            &url.URL{Path: "/.pomerium/callback"},
		refreshCooldown:        opts.RefreshCooldown,
		sessionStore:           cookieStore,
		signingKey:             opts.SigningKey,
		templates:              templates.New(),
	}
	// DeepCopy urls to avoid accidental mutation, err checked in validate func
	p.authenticateURL, _ = urlutil.DeepCopy(opts.AuthenticateURL)
	p.authorizeURL, _ = urlutil.DeepCopy(opts.AuthorizeURL)

	if err := p.UpdatePolicies(&opts); err != nil {
		return nil, err
	}
	metrics.AddPolicyCountCallback("proxy", func() int64 {
		return int64(len(p.routeConfigs))
	})
	p.AuthorizeClient, err = clients.NewAuthorizeClient("grpc",
		&clients.Options{
			Addr:                    p.authorizeURL,
			OverrideCertificateName: opts.OverrideCertificateName,
			SharedSecret:            opts.SharedKey,
			CA:                      opts.CA,
			CAFile:                  opts.CAFile,
			RequestTimeout:          opts.GRPCClientTimeout,
			ClientDNSRoundRobin:     opts.GRPCClientDNSRoundRobin,
		})
	return p, err
}

// UpdatePolicies updates the handlers based on the configured policies
func (p *Proxy) UpdatePolicies(opts *config.Options) error {
	routeConfigs := make(map[string]*routeConfig, len(opts.Policies))
	if len(opts.Policies) == 0 {
		log.Warn().Msg("proxy: configuration has no policies")
	}
	for _, policy := range opts.Policies {
		if err := policy.Validate(); err != nil {
			return fmt.Errorf("proxy: couldn't update policies %s", err)
		}
		proxy := NewReverseProxy(policy.Destination)
		// build http transport (roundtripper) middleware chain
		transport := http.DefaultTransport.(*http.Transport).Clone()
		c := tripper.NewChain()
		c = c.Append(metrics.HTTPMetricsRoundTripper("proxy", policy.Destination.Host))

		var tlsClientConfig tls.Config
		var isCustomClientConfig bool
		if policy.TLSSkipVerify {
			tlsClientConfig.InsecureSkipVerify = true
			isCustomClientConfig = true
			log.Warn().Str("to", policy.Source.String()).Msg("proxy: tls skip verify")
		}
		if policy.RootCAs != nil {
			tlsClientConfig.RootCAs = policy.RootCAs
			isCustomClientConfig = true
			log.Debug().Str("to", policy.Source.String()).Msg("proxy: custom root ca")
		}

		if policy.ClientCertificate != nil {
			tlsClientConfig.Certificates = []tls.Certificate{*policy.ClientCertificate}
			isCustomClientConfig = true
			log.Debug().Str("to", policy.Source.String()).Msg("proxy: client certs enabled")
		}

		if policy.TLSServerName != "" {
			tlsClientConfig.ServerName = policy.TLSServerName
			isCustomClientConfig = true
			log.Debug().Str("to", policy.Source.String()).Msgf("proxy: tls hostname override to: %s", policy.TLSServerName)
		}

		// We avoid setting a custom client config unless we have to as
		// if TLSClientConfig is nil, the default configuration is used.
		if isCustomClientConfig {
			transport.TLSClientConfig = &tlsClientConfig
		}
		proxy.Transport = c.Then(transport)

		handler, err := p.newReverseProxyHandler(proxy, &policy)
		if err != nil {
			return err
		}
		routeConfigs[policy.Source.Host] = &routeConfig{
			mux:    handler,
			policy: policy,
		}
	}
	p.routeConfigs = routeConfigs
	return nil
}

// UpstreamProxy stores information for proxying the request to the upstream.
type UpstreamProxy struct {
	name    string
	handler http.Handler
}

// ServeHTTP handles the second (reverse-proxying) leg of pomerium's request flow
func (u *UpstreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), fmt.Sprintf("%s%s", r.Host, r.URL.Path))
	defer span.End()
	u.handler.ServeHTTP(w, r.WithContext(ctx))
}

// NewReverseProxy returns a new ReverseProxy that routes URLs to the scheme, host, and
// base path provided in target. NewReverseProxy rewrites the Host header.
func NewReverseProxy(to *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(to)
	sublogger := log.With().Str("proxy", to.Host).Logger()
	proxy.ErrorLog = stdlog.New(&log.StdLogWrapper{Logger: &sublogger}, "", 0)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		// Identifies the originating IP addresses of a client connecting to
		// a web server through an HTTP proxy or a load balancer.
		req.Header.Add("X-Forwarded-Host", req.Host)
		director(req)
		req.Host = to.Host
	}
	return proxy
}

// newReverseProxyHandler applies handler specific options to a given route.
func (p *Proxy) newReverseProxyHandler(rp *httputil.ReverseProxy, route *config.Policy) (handler http.Handler, err error) {
	handler = &UpstreamProxy{
		name:    route.Destination.Host,
		handler: rp,
	}
	c := middleware.NewChain()
	c = c.Append(middleware.StripPomeriumCookie(p.cookieName))

	// if signing key is set, add signer to middleware
	if len(p.signingKey) != 0 {
		signer, err := cryptutil.NewES256Signer(p.signingKey, route.Source.Host)
		if err != nil {
			return nil, err
		}
		c = c.Append(middleware.SignRequest(signer, HeaderUserID, HeaderEmail, HeaderGroups, HeaderJWT))
	}
	// websockets cannot use the non-hijackable timeout-handler
	if !route.AllowWebsockets {
		timeout := p.defaultUpstreamTimeout
		if route.UpstreamTimeout != 0 {
			timeout = route.UpstreamTimeout
		}
		timeoutMsg := fmt.Sprintf("%s failed to respond within the %s timeout period", route.Destination.Host, timeout)
		handler = http.TimeoutHandler(handler, timeout, timeoutMsg)
	}

	return c.Then(handler), nil
}

// UpdateOptions updates internal structures based on config.Options
func (p *Proxy) UpdateOptions(o config.Options) error {
	if p == nil {
		return nil
	}
	log.Info().Msg("proxy: updating options")
	return p.UpdatePolicies(&o)
}
