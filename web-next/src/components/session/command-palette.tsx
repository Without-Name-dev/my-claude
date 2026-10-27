import React from 'react';
import { executeCommand } from '@/api/config-api';

interface CommandPaletteProps {
  sessionId: string;
  isOpen: boolean;
  onClose: () => void;
}

export function CommandPalette({ sessionId, isOpen, onClose }: CommandPaletteProps) {
  const [search, setSearch] = React.useState('');
  const [selectedIndex, setSelectedIndex] = React.useState(0);
  const [executing, setExecuting] = React.useState(false);

  const filteredCommands = React.useMemo(() => {
    if (!search) return commands;
    const lower = search.toLowerCase();
    return commands.filter(
      cmd => cmd.name.toLowerCase().includes(lower) ||
             cmd.description.toLowerCase().includes(lower) ||
             cmd.syntax.toLowerCase().includes(lower)
    );
  }, [search]);

  React.useEffect(() => {
    if (!isOpen) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
      } else if (e.key === 'ArrowDown') {
        e.preventDefault();
        setSelectedIndex(i => Math.min(i + 1, filteredCommands.length - 1));
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        setSelectedIndex(i => Math.max(i - 1, 0));
      } else if (e.key === 'Enter' && filteredCommands[selectedIndex]) {
        e.preventDefault();
        handleExecute(filteredCommands[selectedIndex]);
      }
    };

    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, selectedIndex, filteredCommands, onClose]);

  const handleExecute = async (command: Command) => {
    setExecuting(true);
    try {
      await executeCommand(sessionId, command.syntax);
      onClose();
    } catch (error) {
      console.error('Command execution failed:', error);
    } finally {
      setExecuting(false);
    }
  };

  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-50 bg-black/50 flex items-start justify-center pt-[20vh] p-4">
      <div className="bg-white rounded-lg shadow-2xl w-full max-w-2xl overflow-hidden">
        <div className="p-4 border-b">
          <input
            type="text"
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="搜索命令... (Cmd+K)"
            className="w-full px-4 py-2 border rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
            autoFocus
          />
        </div>

        <div className="max-h-96 overflow-y-auto">
          {filteredCommands.length === 0 ? (
            <div className="p-8 text-center text-gray-500">
              没有找到匹配的命令
            </div>
          ) : (
            filteredCommands.map((cmd, index) => (
              <button
                key={cmd.id}
                onClick={() => handleExecute(cmd)}
                disabled={executing}
                className={`w-full px-4 py-3 flex items-start gap-3 text-left transition-colors ${
                  index === selectedIndex ? 'bg-blue-50' : 'hover:bg-gray-50'
                } ${executing ? 'opacity-50 cursor-not-allowed' : ''}`}
              >
                <span className="text-2xl">{cmd.icon}</span>
                <div className="flex-1">
                  <div className="flex items-center justify-between mb-1">
                    <span className="font-medium text-gray-900">{cmd.name}</span>
                    <code className="text-xs px-2 py-1 bg-gray-100 rounded">
                      {cmd.syntax}
                    </code>
                  </div>
                  <p className="text-sm text-gray-600">{cmd.description}</p>
                </div>
              </button>
            ))
          )}
        </div>

        <div className="p-3 bg-gray-50 text-xs text-gray-500 flex items-center justify-between border-t">
          <div className="flex gap-4">
            <span><kbd className="px-1.5 py-0.5 bg-white border rounded">↑↓</kbd> 导航</span>
            <span><kbd className="px-1.5 py-0.5 bg-white border rounded">Enter</kbd> 执行</span>
            <span><kbd className="px-1.5 py-0.5 bg-white border rounded">Esc</kbd> 关闭</span>
          </div>
          {executing && <span className="text-blue-600">执行中...</span>}
        </div>
      </div>
    </div>
  );
}

interface Command {
  id: string;
  name: string;
  syntax: string;
  description: string;
  icon: string;
}

const commands: Command[] = [
  {
    id: 'effort',
    name: '设置推理级别',
    syntax: '/effort high',
    description: '设置 AI 推理努力程度 (low/medium/high/xhigh/max/auto)',
    icon: '🧠'
  },
  {
    id: 'model',
    name: '切换模型',
    syntax: '/model opus',
    description: '切换 AI 模型 (opus/sonnet/haiku)',
    icon: '🤖'
  },
  {
    id: 'fast',
    name: '快速模式',
    syntax: '/fast',
    description: '切换快速响应模式',
    icon: '⚡'
  },
  {
    id: 'auto',
    name: '自动模式',
    syntax: '/auto mode on',
    description: '开启/关闭自动执行模式',
    icon: '🔄'
  },
  {
    id: 'compact',
    name: '压缩对话',
    syntax: '/compact',
    description: '压缩历史对话以释放空间',
    icon: '📦'
  },
  {
    id: 'clear',
    name: '清空会话',
    syntax: '/clear',
    description: '清空当前会话内容',
    icon: '🗑️'
  },
  {
    id: 'help',
    name: '显示帮助',
    syntax: '/help',
    description: '显示所有可用命令',
    icon: '❓'
  },
  {
    id: 'config',
    name: '查看配置',
    syntax: '/config',
    description: '查看当前会话配置',
    icon: '⚙️'
  },
  {
    id: 'workflows',
    name: '工作流',
    syntax: '/workflows',
    description: '查看运行中的工作流',
    icon: '📊'
  }
];

// Hook for keyboard shortcut
export function useCommandPalette() {
  const [isOpen, setIsOpen] = React.useState(false);

  React.useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        setIsOpen(prev => !prev);
      }
    };

    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, []);

  return { isOpen, setIsOpen };
}
