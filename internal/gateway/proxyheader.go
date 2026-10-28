package gateway

import (
	"net/http"
	"net/netip"
	"strings"
)

// proxyHeaderAuthenticator trusts an identity a front proxy has already verified
// and passes as request headers (issue #9, ADR-0020). It is the smallest way to
// put any auth proxy — oauth2-proxy, Cloudflare Access, a JWT-validating gateway
// — in front of DCMS with no Go required.
//
// SECURITY: the headers are honoured only on connections from a trusted proxy
// (server.trusted_proxies; default loopback + private networks), so a caller who
// reaches the DCMS port directly can't set them and impersonate anyone. The proxy
// must still strip UserHeader/RolesHeader from inbound client requests and re-set
// them itself. Selecting it is an explicit opt-in (auth.provider: proxy_header).
type proxyHeaderAuthenticator struct {
	userHeader  string
	rolesHeader string
	rolesSep    string
	trusted     []netip.Prefix // nil ⇒ defaultTrustedProxies
}

// NewProxyHeaderAuthenticator builds the proxy-header Authenticator. userHeader
// is required; rolesHeader is optional; rolesSep defaults to ","; trusted lists
// the proxies whose identity headers are believed (nil ⇒ loopback + private
// networks, the same default as server.trusted_proxies).
func NewProxyHeaderAuthenticator(userHeader, rolesHeader, rolesSep string, trusted []netip.Prefix) Authenticator {
	if rolesSep == "" {
		rolesSep = ","
	}
	return &proxyHeaderAuthenticator{
		userHeader:  userHeader,
		rolesHeader: rolesHeader,
		rolesSep:    rolesSep,
		trusted:     trusted,
	}
}

// Authenticate maps the identity header to a principal. An absent user header
// yields the anonymous principal (not an error), mirroring the session source —
// so public routes still work and access rules make the deny decision. So does a
// header on a connection that isn't from a trusted proxy: it is ignored, never
// believed.
func (a *proxyHeaderAuthenticator) Authenticate(r *http.Request) (principal, error) {
	if peer, _ := peerAddr(r); !inTrustedRanges(a.trusted, peer) {
		return principal{}, nil
	}
	id := strings.TrimSpace(r.Header.Get(a.userHeader))
	if id == "" {
		return principal{}, nil
	}
	var roles []string
	if a.rolesHeader != "" {
		for _, role := range strings.Split(r.Header.Get(a.rolesHeader), a.rolesSep) {
			if role = strings.TrimSpace(role); role != "" {
				roles = append(roles, role)
			}
		}
	}
	return principal{ID: id, Roles: roles, Authenticated: true}, nil
}
