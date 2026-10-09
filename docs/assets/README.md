# README illustrations

[English](README.md) | [简体中文](README.zh-CN.md)

| Asset | Used by |
|:--|:--|
| `logo.png` | Brand banner in both READMEs. |
| `architecture-zh-CN.svg` | System architecture in the Chinese README. |
| `architecture-en.svg` | System architecture in the English README. |

## Update the diagrams

From the repository root:

```bash
python3 docs/assets/generate_diagrams.py
```

The generator uses Python's standard library and produces the two architecture SVGs. Edit their text and layout in the script, keeping both languages aligned. The supplied `logo.png` is used directly.

The architecture diagrams follow the Framework components and the system model in `semantic-docs/docs/architecture/`. Each SVG includes an accessible title and description.
