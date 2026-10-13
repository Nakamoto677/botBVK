package postgres

import (
	"context"
	"errors"
	"fmt"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Bootstrap keeps the legacy programmatic interface used by existing callers.
func (s *Store) Bootstrap(ctx context.Context, adminID int64, names []string) error {
	companies := make([]domain.CompanyDefinition, 0, len(names))
	for _, name := range names {
		companies = append(companies, domain.LegacyCompany(name))
	}
	return s.BootstrapCompanies(ctx, adminID, companies)
}

func (s *Store) BootstrapCompanies(ctx context.Context, adminID int64, companies []domain.CompanyDefinition) error {
	if err := domain.ValidateCompanies(companies); err != nil {
		return err
	}
	return s.Transact(ctx, func(t domain.Tx) error {
		tx := t.(*transaction).tx
		// Lock all catalog writes; registration locks its chosen row FOR SHARE.
		if _, err := tx.Exec(ctx, "LOCK TABLE bvk_companies IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE bvk_companies SET registration_active=false"); err != nil {
			return err
		}
		used := map[int64]bool{}
		for position, c := range companies {
			var id int64
			err := tx.QueryRow(ctx, "SELECT id FROM bvk_companies WHERE config_key=$1", c.Key).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				oldName := c.LegacyName
				if c.Key == domain.LegacyCompany(c.Name).Key {
					oldName = c.Name
				}
				if oldName != "" {
					// Exact, explicit migration only; never adopt an unrelated stable ID.
					rows, e := tx.Query(ctx, "SELECT id FROM bvk_companies WHERE name=$1 AND (config_key IS NULL OR config_key=$2)", oldName, domain.LegacyCompany(oldName).Key)
					if e != nil {
						return e
					}
					count := 0
					for rows.Next() {
						if e = rows.Scan(&id); e != nil {
							rows.Close()
							return e
						}
						count++
					}
					e = rows.Err()
					rows.Close()
					if e != nil {
						return e
					}
					if count > 1 || (count == 0 && c.LegacyName != "") {
						return fmt.Errorf("company %q: legacy_name %q must identify exactly one legacy row", c.Key, oldName)
					}
				}
				if id == 0 {
					if err = tx.QueryRow(ctx, "INSERT INTO bvk_companies(name,config_key) VALUES($1,$2) RETURNING id", c.Name, c.Key).Scan(&id); err != nil {
						return err
					}
				}
			} else if err != nil {
				return err
			}
			if used[id] {
				return fmt.Errorf("company %q maps to an already selected legacy row", c.Key)
			}
			used[id] = true
			if _, err = tx.Exec(ctx, "UPDATE bvk_companies SET name=$2,config_key=$3,registration_active=true,registration_order=$4 WHERE id=$1", id, c.Name, c.Key, position); err != nil {
				return err
			}
		}
		if adminID == 0 {
			return nil
		}
		_, err := t.GetUser(ctx, adminID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return t.SaveUser(ctx, domain.User{ID: adminID, Name: "Administrator", Role: "admin", Approved: true})
	})
}

// Company resolves historical references regardless of registration availability.
func (s *Store) Company(ctx context.Context, id int64) (domain.Company, error) {
	var c domain.Company
	err := s.pool.QueryRow(ctx, "SELECT id,name FROM bvk_companies WHERE id=$1", id).Scan(&c.ID, &c.Name)
	return c, mapError(err)
}
