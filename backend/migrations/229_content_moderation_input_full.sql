-- 风控中心：为 cyber_policy 审计记录补充完整输入文本字段

ALTER TABLE content_moderation_logs
ADD COLUMN IF NOT EXISTS input_full TEXT NOT NULL DEFAULT '';
