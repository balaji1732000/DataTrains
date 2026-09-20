package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

type Invitation struct {
	ID, OrganizationID, Email, Role string
	ExpiresAt                       time.Time
}

type InvitationAcceptance struct {
	AccountID, OrganizationID, Role string
	ContributorID                   *string
}

func (store *Store) CreateInvitation(ctx context.Context, organizationID, email, role, actor, requestID string, expiresAt, now time.Time) (Invitation, string, error) {
	identifier, err := newID("invite")
	if err != nil {
		return Invitation{}, "", err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Invitation{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	digest := sha256.Sum256([]byte(token))
	invitation := Invitation{
		ID: identifier, OrganizationID: organizationID, Email: strings.ToLower(strings.TrimSpace(email)),
		Role: role, ExpiresAt: expiresAt.UTC(),
	}
	err = store.withAudit(ctx, domain.AuditEvent{
		Actor: actor, Action: "invitation.created", Resource: "invitation/" + identifier,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": organizationID, "role": role},
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO invitations (id, organization_id, role, email, token_hash, invited_by, expires_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, identifier, organizationID, role, invitation.Email,
			hex.EncodeToString(digest[:]), actor, invitation.ExpiresAt, now.UTC())
		return err
	})
	return invitation, token, err
}

func (store *Store) AcceptInvitation(ctx context.Context, token, issuer, subject, email, displayName, requestID string, emailVerified bool, now time.Time) (InvitationAcceptance, error) {
	if token == "" || issuer == "" || subject == "" || !emailVerified {
		return InvitationAcceptance{}, ErrInvitationInvalid
	}
	digest := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(digest[:])
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return InvitationAcceptance{}, err
	}
	defer tx.Rollback(ctx)

	var invitation Invitation
	err = tx.QueryRow(ctx, `
		SELECT id, organization_id, email, role, expires_at
		FROM invitations
		WHERE token_hash=$1 AND status='pending' AND expires_at>$2
		FOR UPDATE`, tokenHash, now.UTC()).Scan(
		&invitation.ID, &invitation.OrganizationID, &invitation.Email, &invitation.Role, &invitation.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvitationAcceptance{}, ErrInvitationInvalid
	}
	if err != nil {
		return InvitationAcceptance{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(email), invitation.Email) {
		return InvitationAcceptance{}, ErrInvitationInvalid
	}

	var result InvitationAcceptance
	result.OrganizationID, result.Role = invitation.OrganizationID, invitation.Role
	err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE lower(email)=lower($1) FOR UPDATE`, invitation.Email).Scan(&result.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		result.AccountID, err = newID("acct")
		if err != nil {
			return InvitationAcceptance{}, err
		}
		if strings.TrimSpace(displayName) == "" {
			displayName = invitation.Email
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, display_name, email, created_at) VALUES ($1,$2,$3,$4)`, result.AccountID, displayName, invitation.Email, now.UTC()); err != nil {
			return InvitationAcceptance{}, err
		}
	} else if err != nil {
		return InvitationAcceptance{}, err
	}

	identityID, err := newID("ident")
	if err != nil {
		return InvitationAcceptance{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO oidc_identities (id, account_id, issuer, subject, email_at_link_time, email_verified_at_link_time, created_at, last_authenticated_at)
		VALUES ($1,$2,$3,$4,$5,true,$6,$6)`, identityID, result.AccountID, issuer, subject, invitation.Email, now.UTC()); err != nil {
		return InvitationAcceptance{}, ErrInvitationInvalid
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_memberships (organization_id, account_id, role, status, created_at)
		VALUES ($1,$2,$3,'active',$4)
		ON CONFLICT (organization_id, account_id, role) DO UPDATE SET status='active'`,
		invitation.OrganizationID, result.AccountID, invitation.Role, now.UTC()); err != nil {
		return InvitationAcceptance{}, err
	}
	if invitation.Role == "contributor" {
		var contributorID string
		err := tx.QueryRow(ctx, `SELECT id FROM contributors WHERE account_id=$1`, result.AccountID).Scan(&contributorID)
		if errors.Is(err, pgx.ErrNoRows) {
			contributorID, err = newID("contrib")
			if err != nil {
				return InvitationAcceptance{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO contributors (id, display_name, account_id, created_at) VALUES ($1,$2,$3,$4)`, contributorID, displayName, result.AccountID, now.UTC()); err != nil {
				return InvitationAcceptance{}, err
			}
		} else if err != nil {
			return InvitationAcceptance{}, err
		}
		result.ContributorID = &contributorID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE invitations SET status='accepted', accepted_by=$1, accepted_at=$2 WHERE id=$3`,
		result.AccountID, now.UTC(), invitation.ID); err != nil {
		return InvitationAcceptance{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: result.AccountID, Action: "invitation.accepted", Resource: "invitation/" + invitation.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": invitation.OrganizationID, "role": invitation.Role},
	}); err != nil {
		return InvitationAcceptance{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InvitationAcceptance{}, err
	}
	return result, nil
}
