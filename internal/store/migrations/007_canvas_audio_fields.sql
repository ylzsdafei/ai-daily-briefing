-- v1.1: v1.1-canvas-voice — canvas 洞察流程图 + 罗永浩风格语音播报
-- 两个功能都挂在 issue_insights 上（同一 issue 的扩展产物，和 industry_md / our_md 平级）。
-- 所有三列默认 NULL/空字符串；feature flag off 时完全不写，零影响老数据。
ALTER TABLE issue_insights ADD COLUMN canvas_json TEXT;
ALTER TABLE issue_insights ADD COLUMN audio_script_md TEXT;
ALTER TABLE issue_insights ADD COLUMN audio_url TEXT;
