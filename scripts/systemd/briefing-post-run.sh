#!/bin/bash
# briefing-post-run.sh
#
# 在 briefing-orchestrator.sh 的 main_loop 成功返回后调用. 做两件事:
#   (1) push ai-daily-site (Hugo 内容) → GitHub Pages
#   (2) push briefing-v3 state (dedup / daily archive / slack payload) → GitHub
#
# 为什么要 push briefing-v3 state:
#   - claude-4 宿主若丢失 (VPS 到期 / 被封 / 磁盘坏), 本地只剩 GitHub 上的数据
#   - dedup 文件 (data/sent_urls.txt, data/sent_titles.txt) 是单点关键状态,
#     丢了会造成老内容被重推, 必须有线上副本
#   - 灾备恢复时 git clone 就能拉到最近一天的状态, 不依赖 Syncthing
#
# fail-soft: 任何 git 操作失败都只 log 不 exit 非零, 保证 orchestrator 看到 0.
# post-run 失败是次要问题, 今日报已推成功才是主要目标.

set -uo pipefail

BRIEFING_ROOT=${BRIEFING_ROOT:-/root/briefing-v3}
BRIEFING_DB=${BRIEFING_DB:-$BRIEFING_ROOT/data/briefing.db}
SITE_DIR=${SITE_DIR:-/root/ai-daily-site}

# 允许测试覆盖 push 命令 (test_orchestrator.sh 会 set 成 echo)
GIT_PUSH_CMD=${GIT_PUSH_CMD:-"git push origin main"}

TODAY=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
STATUS=$(sqlite3 "$BRIEFING_DB" \
    "SELECT status FROM issues WHERE issue_date='$TODAY' ORDER BY id DESC LIMIT 1;" 2>/dev/null)

if [[ "$STATUS" != "published" ]]; then
    echo "[post-run] issue for $TODAY status='$STATUS' (not published), skipping all pushes"
    exit 0
fi

# ---------------------------------------------------------------------
# (1) ai-daily-site: 内容站 push
# ---------------------------------------------------------------------
push_site() {
    if [[ ! -d "$SITE_DIR" ]]; then
        echo "[post-run] site dir $SITE_DIR not found, skipping site push"
        return
    fi
    cd "$SITE_DIR" || return
    if git diff --quiet && git diff --cached --quiet \
        && [ -z "$(git ls-files --others --exclude-standard)" ]; then
        echo "[post-run] no changes in ai-daily-site, skipping push"
        return
    fi
    git add content/ static/images/ 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[post-run] ai-daily-site: nothing staged, skipping commit"
        return
    fi
    git commit -m "chore(content): 自动同步 $TODAY 日报" >/dev/null 2>&1
    if $GIT_PUSH_CMD 2>&1; then
        echo "[post-run] ai-daily-site pushed OK ($TODAY)"
    else
        echo "[post-run] WARN: ai-daily-site push failed (non-fatal)"
    fi
}

# ---------------------------------------------------------------------
# (2) briefing-v3: state + archive push
# ---------------------------------------------------------------------
# 只 stage 白名单文件, 避免把大/临时文件 (backups/*.db, export/, slack-payload
# JSON 历史堆积) 误提交. data/backups 已在 .gitignore 排除.
push_briefing_state() {
    cd "$BRIEFING_ROOT" || return

    local paths=()
    [[ -f "daily/$TODAY.md" ]] && paths+=("daily/$TODAY.md")
    [[ -f "data/sent_urls.txt" ]] && paths+=("data/sent_urls.txt")
    [[ -f "data/sent_titles.txt" ]] && paths+=("data/sent_titles.txt")
    [[ -f "data/slack-payload-$TODAY.json" ]] && paths+=("data/slack-payload-$TODAY.json")

    if [[ ${#paths[@]} -eq 0 ]]; then
        echo "[post-run] no whitelisted state files to commit"
        return
    fi

    git add -- "${paths[@]}" 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[post-run] briefing-v3: no state changes, skipping push"
        return
    fi

    if git commit -m "chore(state): auto-commit $TODAY daily state

Automated post-run commit by briefing-post-run.sh.
Includes today's archive + dedup markers so GitHub has
a disaster-recovery copy of pipeline state.
" >/dev/null 2>&1; then
        if $GIT_PUSH_CMD 2>&1; then
            echo "[post-run] briefing-v3 state pushed OK ($TODAY)"
        else
            echo "[post-run] WARN: briefing-v3 state push failed (non-fatal)"
        fi
    else
        echo "[post-run] briefing-v3: commit no-op (nothing staged)"
    fi
}

push_site
push_briefing_state

exit 0
