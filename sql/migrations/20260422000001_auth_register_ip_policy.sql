-- +goose Up
-- +goose StatementBegin

ALTER TABLE `users`
    ADD COLUMN `register_ip` VARCHAR(64) NOT NULL DEFAULT '' AFTER `version`,
    ADD KEY `idx_register_ip_created` (`register_ip`, `created_at`);

UPDATE `users`
SET `register_ip` = `last_login_ip`
WHERE `register_ip` = '' AND `last_login_ip` <> '';

INSERT INTO `system_settings` (`k`, `v`, `description`) VALUES
    ('auth.register_ip_cooldown_sec', '600', '同一 IP 两次注册之间的冷却秒数;0=关闭'),
    ('auth.register_ip_max_per_24h', '3', '同一 IP 24 小时内允许注册的最大次数;0=关闭')
ON DUPLICATE KEY UPDATE `k` = VALUES(`k`);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM `system_settings`
WHERE `k` IN ('auth.register_ip_cooldown_sec', 'auth.register_ip_max_per_24h');

ALTER TABLE `users`
    DROP INDEX `idx_register_ip_created`,
    DROP COLUMN `register_ip`;

-- +goose StatementEnd
