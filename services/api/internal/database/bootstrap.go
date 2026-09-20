package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

type AdministratorBootstrap struct {
	Issuer, Subject, Email, DisplayName, OrganizationName string
}

type AdministratorBootstrapResult struct {
	AccountID, OrganizationID string
}

func (store *Store) BootstrapAdministrator(ctx context.Context, input AdministratorBootstrap, requestID string, now time.Time) (AdministratorBootstrapResult, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdministratorBootstrapResult{}, err
	}
	defer tx.Rollback(ctx)

	var result AdministratorBootstrapResult
	err = tx.QueryRow(ctx, `
		SELECT a.id
		FROM oidc_identities i JOIN accounts a ON a.id=i.account_id
		WHERE i.issuer=$1 AND i.subject=$2
		FOR UPDATE OF a, i`, input.Issuer, input.Subject).Scan(&result.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		result.AccountID, err = newID("acct")
		if err != nil {
			return AdministratorBootstrapResult{}, err
		}
		identityID, err := newID("ident")
		if err != nil {
			return AdministratorBootstrapResult{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, display_name, email, created_at) VALUES ($1,$2,$3,$4)`, result.AccountID, input.DisplayName, input.Email, now.UTC()); err != nil {
			return AdministratorBootstrapResult{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO oidc_identities (id, account_id, issuer, subject, email_at_link_time, created_at) VALUES ($1,$2,$3,$4,$5,$6)`, identityID, result.AccountID, input.Issuer, input.Subject, input.Email, now.UTC()); err != nil {
			return AdministratorBootstrapResult{}, err
		}
	} else if err != nil {
		return AdministratorBootstrapResult{}, err
	}

	err = tx.QueryRow(ctx, `SELECT id FROM organizations WHERE name=$1 ORDER BY created_at, id LIMIT 1 FOR UPDATE`, input.OrganizationName).Scan(&result.OrganizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		result.OrganizationID, err = newID("org")
		if err != nil {
			return AdministratorBootstrapResult{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ($1,$2,$3)`, result.OrganizationID, input.OrganizationName, now.UTC()); err != nil {
			return AdministratorBootstrapResult{}, err
		}
	} else if err != nil {
		return AdministratorBootstrapResult{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_memberships (organization_id, account_id, role, status, created_at)
		VALUES ($1,$2,'admin','active',$3)
		ON CONFLICT (organization_id, account_id, role)
		DO UPDATE SET status='active'`, result.OrganizationID, result.AccountID, now.UTC()); err != nil {
		return AdministratorBootstrapResult{}, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: "system/bootstrap-admin", Action: "administrator.bootstrapped",
		Resource: "account/" + result.AccountID, Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"organization_id": result.OrganizationID, "issuer": input.Issuer},
	}); err != nil {
		return AdministratorBootstrapResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdministratorBootstrapResult{}, err
	}
	return result, nil
}
