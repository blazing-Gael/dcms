package gateway

import (
	"net/http"
	"strings"
)

// proxyHeaderAuthenticator trusts an identity a front proxy has already verified
// and passes as request headers (issue #9, ADR-0020). It is the smallest way to
// put any auth proxy — oauth2-proxy, Cloudflare Access, a JWT-validating gateway
// — in front of DCMS with no Go required.
//
// SECURITY: this trusts the headers verbatim. It is safe ONLY behind a proxy you
// control that strips UserHeader/RolesHeader from inbound client requests and
// re-sets them itself; otherwise any caller can set the header and impersonate
// anyone. Selecting it is an explicit opt-in (auth.provider: proxy_header), and
// the same trust model as server.trust_proxy applies.
type proxyHeaderAuthenticator struct {
	userHeader  string
	rolesHeader string
	rolesSep    string
}

// NewProxyHeaderAuthenticator builds the proxy-header Authenticator. userHeader
// is required; rolesHeader is optional; rolesSep defaults to ",".
func NewProxyHeaderAuthenticator(userHeader, rolesHeader, rolesSep string) Authenticator {
	if rolesSep == "" {
		rolesSep = ","
	}
	return &proxyHeaderAuthenticator{
		userHeader:  userHeader,
		rolesHeader: rolesHeader,
		rolesSep:    rolesSep,
	}
}

// Authenticate maps the identity header to a principal. An absent user header
// yields the anonymous principal (not an error), mirroring the session source —
// so public routes still work and access rules make the deny decision.
func (a *proxyHeaderAuthenticator) Authenticate(r *http.Request) (principal, error) {
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
