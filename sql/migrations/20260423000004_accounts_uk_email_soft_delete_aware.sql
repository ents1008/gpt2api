-- +goose Up
-- +goose StatementBegin
--
-- 修复:批量软删账号后再导入同 email 报 "Duplicate entry ... for key uk_email"。
--
-- 根因:原表 UNIQUE KEY uk_email (email) 对 email 列做了纯唯一约束,
--       但业务上 DELETE 走软删(仅置 deleted_at),不物理删行,
--       导致被删行仍占着这个 email 的唯一槽位,再导入同 email 立刻 1062。
--
-- MySQL 没有 Postgres 那种 partial unique index(WHERE deleted_at IS NULL),
-- 标准绕法是:
--   1) 加一个 STORED 生成列 active_email,值 = 活着时的 email / 软删后 NULL;
--   2) 把唯一约束换到这个生成列上。
-- MySQL 的唯一索引允许多个 NULL 共存,所以:
--   * 活行:active_email = email → 唯一性生效(同一 email 不能同时存在两个活行)
--   * 软删行:active_email = NULL → 互相不冲突,也不跟活行冲突
--   * 再次导入同 email:允许成功(旧行的 email 已被"软删 → NULL"释放掉槽位)
-- 业务代码无需改动。
ALTER TABLE `oai_accounts`
    DROP INDEX `uk_email`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    ADD COLUMN `active_email` VARCHAR(128)
        GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN `email` ELSE NULL END) STORED
        COMMENT '活行=email / 软删行=NULL;供 uk_active_email 做"软删感知"的唯一约束';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    ADD UNIQUE KEY `uk_active_email` (`active_email`);
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    DROP INDEX `uk_active_email`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    DROP COLUMN `active_email`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    ADD UNIQUE KEY `uk_email` (`email`);
-- +goose StatementEnd
