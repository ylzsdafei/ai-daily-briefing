# systemd 调度脚本 (正本)

这里是 `/usr/local/bin/briefing-*.sh` 的 **版本化副本**, 纳入 git 以防宿主
 (claude-4) 丢失. 实际运行的是 `/usr/local/bin/` 下的文件.

## 文件清单

| 文件 | 作用 |
|------|------|
| `briefing-orchestrator.sh` | 主调度: preflight + 带重试的 D4 主循环 |
| `briefing-post-run.sh` | 日报成功后自动 commit+push ai-daily-site + briefing-v3 state |
| `briefing-weekly-post-run.sh` | 周报成功后的 hook |
| `briefing-daily-reset-dedup.sh` | 日常 dedup 重置 |

## 同步规则

**修改工作流**: 先改 `/usr/local/bin/` 下的正在运行版本, 验证跑通, 再
 `cp` 回本目录 + git commit. 不反过来做 (避免 repo 里的未验证版本先
 被部署).

验证 repo 和运行版本一致:

```bash
for f in briefing-orchestrator.sh briefing-post-run.sh \
         briefing-weekly-post-run.sh briefing-daily-reset-dedup.sh; do
    diff "/usr/local/bin/$f" "./$f" && echo "$f: OK" \
        || echo "$f: DRIFT"
done
```

## 灾备恢复

如果宿主挂了, 在新机器上:

```bash
git clone https://github.com/ylzsdafei/ai-daily-briefing.git /root/briefing-v3
cp /root/briefing-v3/scripts/systemd/*.sh /usr/local/bin/
chmod +x /usr/local/bin/briefing-*.sh
# 再恢复 systemd unit + timer, 略
```
