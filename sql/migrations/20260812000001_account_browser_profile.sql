-- +goose Up
-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    ADD COLUMN `browser_profile` JSON NULL AFTER `credential_provenance`;

UPDATE `oai_accounts`
SET `browser_profile` = JSON_OBJECT(
    'version', 1,
    'headers', JSON_OBJECT(
        'Accept-Language', 'zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6',
        'Sec-Ch-Ua', '"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"',
        'Sec-Ch-Ua-Arch', '"x86"',
        'Sec-Ch-Ua-Bitness', '"64"',
        'Sec-Ch-Ua-Full-Version', '"143.0.3650.96"',
        'Sec-Ch-Ua-Full-Version-List', '"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"',
        'Sec-Ch-Ua-Mobile', '?0',
        'Sec-Ch-Ua-Model', '""',
        'Sec-Ch-Ua-Platform', '"Windows"',
        'Sec-Ch-Ua-Platform-Version', '"19.0.0"',
        'User-Agent', 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0'
    ),
    'language', 'zh-CN',
    'timezone', 'Asia/Shanghai'
)
WHERE `browser_profile` IS NULL;

ALTER TABLE `oai_accounts`
    MODIFY COLUMN `browser_profile` JSON NOT NULL AFTER `credential_provenance`;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE `oai_accounts`
    DROP COLUMN `browser_profile`;
-- +goose StatementEnd
