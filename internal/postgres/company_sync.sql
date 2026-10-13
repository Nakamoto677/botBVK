ALTER TABLE bvk_companies ADD COLUMN config_key text;
ALTER TABLE bvk_companies ADD COLUMN registration_active boolean NOT NULL DEFAULT false;
ALTER TABLE bvk_companies ADD COLUMN registration_order integer NOT NULL DEFAULT 0;
ALTER TABLE bvk_companies DROP CONSTRAINT bvk_companies_name_key;
CREATE UNIQUE INDEX bvk_companies_config_key ON bvk_companies(config_key);
