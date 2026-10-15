-- +goose Up
-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    ADD COLUMN `refresh_client_id` VARCHAR(64) NOT NULL DEFAULT '' AFTER `client_id`,
    ADD COLUMN `credential_provenance` VARCHAR(32) NOT NULL DEFAULT '' AFTER `refresh_client_id`,
    ADD COLUMN `admission_state` VARCHAR(16) NOT NULL DEFAULT 'admitted' AFTER `image_quota_updated_at`,
    ADD COLUMN `admission_stage` TINYINT UNSIGNED NOT NULL DEFAULT 3 AFTER `admission_state`,
    ADD COLUMN `admission_started_at` DATETIME NULL AFTER `admission_stage`,
    ADD COLUMN `admission_next_check_at` DATETIME NULL AFTER `admission_started_at`,
    ADD COLUMN `admission_last_check_at` DATETIME NULL AFTER `admission_next_check_at`,
    ADD COLUMN `admission_claimed_until` DATETIME NULL AFTER `admission_last_check_at`,
    ADD COLUMN `admission_reason` VARCHAR(128) NOT NULL DEFAULT '' AFTER `admission_claimed_until`,
    ADD INDEX `idx_account_admission_due` (`admission_state`, `admission_next_check_at`);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    DROP INDEX `idx_account_admission_due`,
    DROP COLUMN `admission_reason`,
    DROP COLUMN `admission_claimed_until`,
    DROP COLUMN `admission_last_check_at`,
    DROP COLUMN `admission_next_check_at`,
    DROP COLUMN `admission_started_at`,
    DROP COLUMN `admission_stage`,
    DROP COLUMN `admission_state`,
    DROP COLUMN `credential_provenance`,
    DROP COLUMN `refresh_client_id`;
-- +goose StatementEnd
