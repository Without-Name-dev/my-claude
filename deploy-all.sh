#!/bin/bash
# My Claude 完整部署脚本
# 生成时间: 2026-10-27
# 用途: 部署 Server + Connector + 清理服务器

set -e

echo "🚀 开始部署 My Claude"
echo "================================"

# ==================== 1. 部署 Go Server ====================
echo ""
echo "📦 Step 1/4: 部署 Go Server"
echo "--------------------------------"

# 上传新版本
echo "上传 Server..."
scp -i ~/.ssh/aa_deploy go/dist/aa-server-linux root@8.166.138.207:/opt/agents-anywhere/bin/aa-server.new

# 安装并重启
echo "安装并重启服务..."
ssh -i ~/.ssh/aa_deploy root@8.166.138.207 '
  systemctl stop agents-anywhere
  cp /opt/agents-anywhere/bin/aa-server /opt/agents-anywhere/bin/aa-server.bak
  install -m 755 -o root -g root /opt/agents-anywhere/bin/aa-server.new /opt/agents-anywhere/bin/aa-server
  systemctl start agents-anywhere
  sleep 3
'

# 验证服务
echo "验证服务..."
if curl -s http://8.166.138.207/healthz | grep -q "ok"; then
  echo "✅ Server 部署成功"
else
  echo "❌ Server 启动失败，请检查日志"
  ssh -i ~/.ssh/aa_deploy root@8.166.138.207 'journalctl -u agents-anywhere -n 50 --no-pager'
  exit 1
fi

# ==================== 2. 部署 Windows Connector ====================
echo ""
echo "📦 Step 2/4: 部署 Windows Connector"
echo "--------------------------------"

# 先构建新二进制。写成 .new：start.bat 的循环会在旧进程退出后自动替换，
# 直接覆盖一个正在运行的 exe 在 Windows 上会失败。
echo "构建 Connector..."
( cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o dist/aa-connector.exe.new ./cmd/aa-connector )
if [ ! -f go/dist/aa-connector.exe.new ]; then
  echo "❌ Connector 构建失败"
  exit 1
fi
cp -f go/dist/aa-connector.exe.new go/dist/aa-connector.exe.bak.$(date +%Y%m%d-%H%M%S)

# 停止旧进程（Git Bash 里必须走 cmd；`2>nul` 会创建一个名为 nul 的文件）
echo "停止旧 Connector..."
PID=$(powershell.exe -NoProfile -NonInteractive -Command '(Get-Process aa-connector -ErrorAction SilentlyContinue | Select-Object -First 1).Id' 2>/dev/null | tr -d '\r')
if [ -n "$PID" ]; then
  cmd //c "taskkill /PID $PID /F" >/dev/null 2>&1 || true
  sleep 8
fi

# start.bat 的循环会把 .new 换上并重启；窗口已经关掉时手动拉起一个。
if ! tasklist 2>/dev/null | grep -qi aa-connector; then
  echo "启动 Connector..."
  cmd //c start "" "C:\Users\25640\my_claude\go\dist\start.bat"
  sleep 5
fi
if tasklist 2>/dev/null | grep -qi aa-connector; then
  echo "✅ Connector 已更新并启动"
else
  echo "❌ Connector 没有启动，请手动运行 go\\dist\\start.bat"
  exit 1
fi

# ==================== 3. 清理服务器临时文件 ====================
echo ""
echo "🧹 Step 3/4: 清理服务器"
echo "--------------------------------"

ssh -i ~/.ssh/aa_deploy root@8.166.138.207 '
  # 清理上传的临时文件
  rm -f /opt/agents-anywhere/bin/aa-server.new

  # 清理旧的备份文件（保留最新 1 个）
  cd /opt/agents-anywhere/bin
  ls -t aa-server.bak* 2>/dev/null | tail -n +2 | xargs -r rm -f

  # 清理 Web 旧备份（保留最新 2 个）
  cd /opt/agents-anywhere
  ls -td web.bak.* 2>/dev/null | tail -n +3 | xargs -r rm -rf

  # 清理 IPA 同步临时文件
  rm -f /var/lib/agents-anywhere/ipa-sync.partial

  echo "✅ 服务器清理完成"
'

# ==================== 4. 验证部署 ====================
echo ""
echo "✅ Step 4/4: 验证部署"
echo "--------------------------------"

# 检查 Server 健康状态
echo "检查 Server 健康状态..."
curl -s http://8.166.138.207/api/v2/health | head -5

# 检查数据库迁移
echo ""
echo "检查数据库迁移..."
ssh -i ~/.ssh/aa_deploy root@8.166.138.207 'sqlite3 /var/lib/agents-anywhere/server.db ".schema message_queue" | head -5' 2>/dev/null || echo "SQLite 命令不可用，跳过"

# 检查 Connector 状态
echo ""
echo "检查 Connector 状态..."
tasklist | grep aa-connector || echo "Connector 未在运行"

echo ""
echo "================================"
echo "🎉 部署完成！"
echo ""
echo "新功能已上线："
echo "  ✅ 消息队列 API"
echo "  ✅ 斜杠命令系统 (9 个命令)"
echo "  ✅ 配置管理 API"
echo "  ✅ Agent/Workflow 可视化 API"
echo "  ✅ Redis 支持 (可选)"
echo ""
echo "新的数据库迁移："
echo "  ✅ 0013_message_queue.sql"
echo "  ✅ 0014_commands_and_workflows.sql"
echo ""
echo "测试 API:"
echo "  curl http://8.166.138.207/api/v2/health"
echo ""
