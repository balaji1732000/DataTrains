package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	ModeLocal = "local"
	ModeOIDC  = "oidc"
)

type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	Name          string
	EmailVerified bool
}

func (identity Identity) AuditActor() string {
	if identity.Issuer == ModeLocal {
		return identity.Subject
	}
	return identity.Issuer + "#" + identity.Subject
}

type Authenticator interface {
	Authenticate(context.Context, *http.Request) (Identity, error)
}

type Config struct {
	Mode     string
	Issuer   string
	Audience string
}

func New(ctx context.Context, config Config) (Authenticator, error) {
	switch config.Mode {
	case ModeLocal:
		return HeaderAuthenticator{}, nil
	case ModeOIDC:
		return NewOIDC(ctx, config.Issuer, config.Audience)
	default:
		return nil, fmt.Errorf("unsupported authentication mode %q", config.Mode)
	}
}

type HeaderAuthenticator struct{}

func (HeaderAuthenticator) Authenticate(_ context.Context, request *http.Request) (Identity, error) {
	actor := strings.TrimSpace(request.Header.Get("X-Actor-ID"))
	if actor == "" {
		return Identity{}, errors.New("X-Actor-ID header is required in local mode")
	}
	return Identity{Issuer: ModeLocal, Subject: actor}, nil
}

type OIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	provider *oidc.Provider
}

func NewOIDC(ctx context.Context, issuer, audience string) (*OIDCAuthenticator, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("OpenID issuer and audience are required")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OpenID provider: %w", err)
	}
	return &OIDCAuthenticator{verifier: provider.Verifier(&oidc.Config{ClientID: audience}), provider: provider}, nil
}

func (authenticator *OIDCAuthenticator) Authenticate(ctx context.Context, request *http.Request) (Identity, error) {
	rawToken, err := bearerToken(request.Header.Get("Authorization"))
	if err != nil {
		return Identity{}, err
	}
	token, err := authenticator.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Identity{}, fmt.Errorf("verify OpenID token: %w", err)
	}
	var claims struct {
		Email         string `json:"email"`
		Name          string `json:"name"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("decode OpenID claims: %w", err)
	}
	if strings.TrimSpace(token.Subject) == "" {
		return Identity{}, errors.New("OpenID token has no subject")
	}
	if request.URL.Path == "/v1/invitations/accept" && (!claims.EmailVerified || strings.TrimSpace(claims.Email) == "") {
		userInfo, err := authenticator.provider.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: rawToken,
			TokenType:   "Bearer",
		}))
		if err != nil {
			return Identity{}, fmt.Errorf("retrieve verified OpenID profile for invitation: %w", err)
		}
		if userInfo.Subject != token.Subject {
			return Identity{}, errors.New("OpenID UserInfo subject does not match access token")
		}
		var profile struct {
			Name string `json:"name"`
		}
		if err := userInfo.Claims(&profile); err != nil {
			return Identity{}, fmt.Errorf("decode OpenID UserInfo claims: %w", err)
		}
		claims.Email = userInfo.Email
		claims.EmailVerified = userInfo.EmailVerified
		if claims.Name == "" {
			claims.Name = profile.Name
		}
	}
	return Identity{
		Issuer: token.Issuer, Subject: token.Subject, Email: claims.Email,
		Name: claims.Name, EmailVerified: claims.EmailVerified,
	}, nil
}

func bearerToken(header string) (string, error) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("Authorization header must contain one Bearer token")
	}
	return parts[1], nil
}
