package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Bootstrap converts name-only companies for tests that do not need stable keys.
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

// CompanyIDByIdentifier resolves a client-group company without relying on a
// numeric row ID. The stable config key wins; an exact unique name is accepted
// for legacy name-only installations.
func (s *Store) CompanyIDByIdentifier(ctx context.Context, value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, domain.ErrInvalid
	}
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT id FROM bvk_companies WHERE config_key=$1", value).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, "SELECT id FROM bvk_companies WHERE name=$1 ORDER BY id LIMIT 2", value)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var candidate int64
		if err = rows.Scan(&candidate); err != nil {
			return 0, err
		}
		ids = append(ids, candidate)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, domain.ErrNotFound
	case 1:
		return ids[0], nil
	default:
		return 0, domain.ErrConflict
	}
}
