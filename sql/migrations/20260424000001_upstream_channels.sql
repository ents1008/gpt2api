-- +goose Up
-- +goose StatementBegin
--
-- 上游渠道表:除内置的 ChatGPT 账号池外,管理员可在后台添加自定义上游渠道
-- (OpenAI 兼容接口、Gemini 兼容接口等)。每个渠道有自己的 base_url、api_key、
-- 倍率、优先级、健康状态。
--
-- 模型映射表:定义"本地模型 slug → 上游模型名"的映射关系,
-- 一个本地模型可以映射到多个渠道(同一行 modality 标记走文字/图片/视频),
-- 调度时按 priority + 渠道健康度挑选。
--
CREATE TABLE `upstream_channels` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `name`             VARCHAR(64)    NOT NULL,
  `type`             VARCHAR(32)    NOT NULL,              -- openai / gemini
  `base_url`         VARCHAR(512)   NOT NULL,
  `api_key_enc`      VARCHAR(2048)  NOT NULL,              -- AES-GCM base64
  `enabled`          TINYINT(1)     NOT NULL DEFAULT 1,
  `priority`         INT            NOT NULL DEFAULT 100,  -- 越小越优先
  `weight`           INT            NOT NULL DEFAULT 1,    -- 同优先级内加权轮询
  `timeout_s`        INT            NOT NULL DEFAULT 120,
  `ratio`            DOUBLE         NOT NULL DEFAULT 1.0,  -- 渠道级倍率(乘算到最终计费)
  `extra`            JSON           NULL,                  -- 自定义 header / proxy 等
  `status`           VARCHAR(16)    NOT NULL DEFAULT 'healthy', -- healthy / unhealthy
  `fail_count`       INT            NOT NULL DEFAULT 0,
  `last_test_at`     DATETIME       NULL,
  `last_test_ok`     TINYINT(1)     NULL,
  `last_test_error`  VARCHAR(512)   NULL DEFAULT '',
  `remark`           VARCHAR(256)   NOT NULL DEFAULT '',
  `created_at`       DATETIME       NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`       DATETIME       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at`       DATETIME       NULL,
  PRIMARY KEY (`id`),
  KEY `idx_enabled_priority` (`enabled`, `priority`),
  KEY `idx_type`             (`type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `channel_model_mappings` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `channel_id`     BIGINT UNSIGNED NOT NULL,
  `local_model`    VARCHAR(128)    NOT NULL,
  `upstream_model` VARCHAR(128)    NOT NULL,
  `modality`       VARCHAR(32)     NOT NULL DEFAULT 'text',   -- text / image / video
  `enabled`        TINYINT(1)      NOT NULL DEFAULT 1,
  `priority`       INT             NOT NULL DEFAULT 100,
  `created_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_channel_local_modality` (`channel_id`, `local_model`, `modality`),
  KEY `idx_local_model`  (`local_model`, `enabled`),
  KEY `idx_channel`      (`channel_id`),
  CONSTRAINT `fk_ch_mapping_channel` FOREIGN KEY (`channel_id`) REFERENCES `upstream_channels`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `channel_model_mappings`;
DROP TABLE IF EXISTS `upstream_channels`;
-- +goose StatementEnd
