# systemd 调度脚本 (正本)

这里是 systemd 相关脚本 / unit / journald 配置的 **版本化副本**, 纳入 git
以防宿主 (claude-4) 丢失. 实际运行的是 `/usr/local/bin/` (脚本) 和
`/etc/systemd/` (unit + drop-in) 下的文件.

## 文件清单

| 文件 | 部署到 | 作用 |
|------|--------|------|
| `briefing-orchestrator.sh` | `/usr/local/bin/` | 主调度: preflight + D4 重试 + selfheal |
| `briefing-post-run.sh` | `/usr/local/bin/` | 日报成功后 push site + wait Pages + promote + push state |
| `briefing-weekly-post-run.sh` | `/usr/local/bin/` | 周报成功后 push site + wait Pages + promote-weekly(+feishu) + push state |
| `briefing-daily-reset-dedup.sh` | `/usr/local/bin/` | 日常 dedup 重置 |
| `briefing-weekly.service` | `/etc/systemd/system/` | weekly oneshot unit (走 dry-run + promote 工作流) |
| `journald-briefing-retention.conf` | `/etc/systemd/journald.conf.d/briefing-retention.conf` | 加大 journal 保留期 (500M / 21d) |

## 同步规则

**修改工作流**: 先改 `/usr/local/bin/` 或 `/etc/systemd/` 下的正在运行
版本, 验证跑通, 再 `cp` 回本目录 + git commit. 不反过来做 (避免 repo
里的未验证版本先被部署).

验证 repo 和运行版本一致:

```bash
for f in briefing-orchestrator.sh briefing-post-run.sh \
         briefing-weekly-post-run.sh briefing-daily-reset-dedup.sh; do
    diff "/usr/local/bin/$f" "./$f" && echo "$f: OK" \
        || echo "$f: DRIFT"
done
diff /etc/systemd/system/briefing-weekly.service ./briefing-weekly.service \
    && echo "weekly.service: OK" || echo "weekly.service: DRIFT"
diff /etc/systemd/journald.conf.d/briefing-retention.conf \
    ./journald-briefing-retention.conf \
    && echo "journald drop-in: OK" || echo "journald drop-in: DRIFT"
```

## 灾备恢复

如果宿主挂了, 在新机器上:

```bash
git clone https://github.com/ylzsdafei/ai-daily-briefing.git /root/briefing-v3
cp /root/briefing-v3/scripts/systemd/*.sh /usr/local/bin/
chmod +x /usr/local/bin/briefing-*.sh
cp /root/briefing-v3/scripts/systemd/briefing-weekly.service /etc/systemd/system/
mkdir -p /etc/systemd/journald.conf.d/
cp /root/briefing-v3/scripts/systemd/journald-briefing-retention.conf \
   /etc/systemd/journald.conf.d/briefing-retention.conf
systemctl daemon-reload
systemctl restart systemd-journald
# 再恢复 daily.service / daily.timer / weekly.timer (尚未入库, 需备份)
```
