#!/bin/bash
# briefing-weekly-post-run.sh
#
# v1.0.1 Phase 4.5 (W1): 周报 systemd ExecStartPost 脚本. systemd 默认只在
# ExecStart 返回 0 时才会跑 ExecStartPost, 所以不需要额外 DB 状态检查 —
# 能跑到这儿说明 briefing weekly 成功产出了 content.
#
# 复用日报 briefing-post-run.sh 的 "只在有变化时 push" 逻辑, 避免空 commit.

set -euo pipefail

SITE_DIR=${SITE_DIR:-/root/ai-daily-site}
GIT_PUSH_CMD=${GIT_PUSH_CMD:-"git push origin main"}

cd "$SITE_DIR" || {
    echo "[weekly post-run] $SITE_DIR not found, skipping"
    exit 0
}

if git diff --quiet && git diff --cached --quiet && [ -z "$(git ls-files --others --exclude-standard)" ]; then
    echo "[weekly post-run] no changes in $SITE_DIR, skipping push"
    exit 0
fi

# Add only content/ and static/images/ (same scope as daily post-run).
git add content/ static/images/ 2>/dev/null || true

WEEK_LABEL=$(date -u '+%Y-W%V')
COMMIT_MSG="chore(content): 自动同步 ${WEEK_LABEL} 周报"

git commit -m "$COMMIT_MSG" 2>&1 || {
    echo "[weekly post-run] nothing to commit, skipping push"
    exit 0
}
$GIT_PUSH_CMD 2>&1
echo "[weekly post-run] ai-daily-site pushed for $WEEK_LABEL"
