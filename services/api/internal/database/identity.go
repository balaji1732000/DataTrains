package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type OrganizationMembership struct {
	OrganizationID string `json:"organization_id"`
	Role           string `json:"role"`
}

type IdentityAccess struct {
	AccountID     string                   `json:"account_id"`
	DisplayName   string                   `json:"display_name"`
	Email         string                   `json:"email,omitempty"`
	ContributorID *string                  `json:"contributor_id,omitempty"`
	Memberships   []OrganizationMembership `json:"memberships"`
}

func (access IdentityAccess) HasRole(role string) bool {
	for _, membership := range access.Memberships {
		if membership.Role == role {
			return true
		}
	}
	return false
}

func (access IdentityAccess) HasOrganizationRole(organizationID string, roles ...string) bool {
	for _, membership := range access.Memberships {
		if membership.OrganizationID != organizationID {
			continue
		}
		for _, role := range roles {
			if membership.Role == role {
				return true
			}
		}
	}
	return false
}

func (access IdentityAccess) OrganizationIDsForRoles(roles ...string) []string {
	seen := make(map[string]bool)
	organizations := make([]string, 0)
	for _, membership := range access.Memberships {
		for _, role := range roles {
			if membership.Role == role && !seen[membership.OrganizationID] {
				seen[membership.OrganizationID] = true
				organizations = append(organizations, membership.OrganizationID)
			}
		}
	}
	return organizations
}

func (store *Store) ResolveOIDCIdentity(ctx context.Context, issuer, subject string, authenticatedAt time.Time) (IdentityAccess, error) {
	var access IdentityAccess
	var email *string
	err := store.pool.QueryRow(ctx, `
		SELECT a.id, a.display_name, a.email, c.id
		FROM oidc_identities i
		JOIN accounts a ON a.id=i.account_id
		LEFT JOIN contributors c ON c.account_id=a.id
		WHERE i.issuer=$1 AND i.subject=$2 AND a.status='active'`, issuer, subject).
		Scan(&access.AccountID, &access.DisplayName, &email, &access.ContributorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityAccess{}, ErrNotFound
	}
	if err != nil {
		return IdentityAccess{}, err
	}
	if email != nil {
		access.Email = *email
	}
	if _, err := store.pool.Exec(ctx, `
		UPDATE oidc_identities SET last_authenticated_at=$3
		WHERE issuer=$1 AND subject=$2
		  AND (last_authenticated_at IS NULL OR last_authenticated_at < $3::timestamptz - interval '5 minutes')`,
		issuer, subject, authenticatedAt.UTC()); err != nil {
		return IdentityAccess{}, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT organization_id, role
		FROM organization_memberships
		WHERE account_id=$1 AND status='active'
		ORDER BY organization_id, role`, access.AccountID)
	if err != nil {
		return IdentityAccess{}, err
	}
	defer rows.Close()
	access.Memberships = make([]OrganizationMembership, 0)
	for rows.Next() {
		var membership OrganizationMembership
		if err := rows.Scan(&membership.OrganizationID, &membership.Role); err != nil {
			return IdentityAccess{}, err
		}
		access.Memberships = append(access.Memberships, membership)
	}
	return access, rows.Err()
}
