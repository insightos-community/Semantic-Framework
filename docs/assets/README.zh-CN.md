# 项目配图

[English](README.md) | [简体中文](README.zh-CN.md)

| 文件 | 用途 |
|:--|:--|
| `logo.png` | 中英文 README 的品牌横幅。 |
| `architecture-zh-CN.svg` | 中文 README 的系统架构图。 |
| `architecture-en.svg` | 英文 README 的系统架构图。 |

## 更新架构图

在仓库根目录执行：

```bash
python3 docs/assets/generate_diagrams.py
```

生成器只使用 Python 标准库，生成中英文两张架构 SVG。文字与布局在脚本中维护，两种语言同步更新。`logo.png` 直接使用提供的原图。

架构图依据 Framework 组件和 `semantic-docs/docs/architecture/` 中的系统模型绘制，每张 SVG 均包含无障碍标题与描述。
