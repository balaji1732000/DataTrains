package authn

import "testing"

func TestBearerToken(t *testing.T) {
	token, err := bearerToken("Bearer signed-token")
	if err != nil || token != "signed-token" {
		t.Fatalf("unexpected token result %q, %v", token, err)
	}
	for _, header := range []string{"", "Basic value", "Bearer", "Bearer one two"} {
		if _, err := bearerToken(header); err == nil {
			t.Fatalf("expected %q to be rejected", header)
		}
	}
}

func TestOIDCAuditActorIncludesIssuer(t *testing.T) {
	identity := Identity{Issuer: "https://identity.example.com", Subject: "user-123"}
	if identity.AuditActor() != "https://identity.example.com#user-123" {
		t.Fatalf("unexpected audit actor %q", identity.AuditActor())
	}
}
