#!/usr/bin/env python3
# Copyright 2026 InsightOS
# SPDX-License-Identifier: Apache-2.0
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""使用 Python 标准库生成 CSV/JSON 的轻量数据画像。"""

from __future__ import annotations

import argparse
import csv
import json
import math
from collections import Counter
from pathlib import Path
from typing import Any


def parse_args() -> argparse.Namespace:
    """解析稳定的脚本参数，所有路径均由调用方在 Project workspace 内提供。"""
    parser = argparse.ArgumentParser(description="分析 CSV/JSON 并输出数据画像")
    parser.add_argument("--input", required=True, help="输入 CSV/JSON 文件")
    parser.add_argument("--json-output", required=True, help="结构化 JSON 输出路径")
    parser.add_argument("--markdown-output", required=True, help="Markdown 输出路径")
    return parser.parse_args()


def load_records(path: Path) -> list[dict[str, Any]]:
    """读取 CSV 或 JSON，并统一为字典记录列表。"""
    suffix = path.suffix.lower()
    if suffix == ".csv":
        with path.open("r", encoding="utf-8-sig", newline="") as handle:
            return [dict(row) for row in csv.DictReader(handle)]
    if suffix == ".json":
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
        if isinstance(value, list):
            records = value
        elif isinstance(value, dict):
            records = [value]
        else:
            raise ValueError("JSON 根必须是对象或对象数组")
        if not all(isinstance(item, dict) for item in records):
            raise ValueError("JSON 数组中的每一项都必须是对象")
        return [dict(item) for item in records]
    raise ValueError("只支持 .csv 和 .json 文件")


def is_missing(value: Any) -> bool:
    """统一识别空值；字符串只把空白视为缺失，不猜测 NA 等业务值。"""
    return value is None or (isinstance(value, str) and value.strip() == "")


def infer_scalar_type(value: Any) -> str:
    """对单个非空值做保守类型推断。"""
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, int):
        return "integer"
    if isinstance(value, float):
        return "number"
    if isinstance(value, (dict, list)):
        return "object"
    text = str(value).strip()
    try:
        int(text)
        return "integer"
    except ValueError:
        pass
    try:
        number = float(text)
        if math.isfinite(number):
            return "number"
    except ValueError:
        pass
    lowered = text.lower()
    if lowered in {"true", "false"}:
        return "boolean"
    return "string"


def numeric_value(value: Any) -> float | None:
    """把可安全解释的有限数值转换为 float。"""
    if isinstance(value, bool) or is_missing(value):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if math.isfinite(number) else None


def profile(records: list[dict[str, Any]], source: Path) -> dict[str, Any]:
    """按字段聚合缺失、类型、唯一值、常见值和数值统计。"""
    columns = sorted({str(key) for row in records for key in row})
    result: dict[str, Any] = {
        "source": str(source),
        "row_count": len(records),
        "column_count": len(columns),
        "columns": {},
    }
    for column in columns:
        values = [row.get(column) for row in records]
        present = [value for value in values if not is_missing(value)]
        type_counts = Counter(infer_scalar_type(value) for value in present)
        serialized = [json.dumps(value, ensure_ascii=False, sort_keys=True) for value in present]
        common = Counter(serialized).most_common(5)
        column_result: dict[str, Any] = {
            "missing_count": len(values) - len(present),
            "non_missing_count": len(present),
            "inferred_types": dict(sorted(type_counts.items())),
            "unique_count": len(set(serialized)),
            "top_values": [
                {"value": json.loads(value), "count": count} for value, count in common
            ],
        }
        numeric = [number for value in present if (number := numeric_value(value)) is not None]
        if numeric:
            column_result["numeric"] = {
                "count": len(numeric),
                "min": min(numeric),
                "max": max(numeric),
                "mean": sum(numeric) / len(numeric),
            }
        result["columns"][column] = column_result
    return result


def render_markdown(result: dict[str, Any]) -> str:
    """把结构化画像转换为便于用户阅读的 Markdown。"""
    lines = [
        "# 数据画像",
        "",
        f"- 来源：`{result['source']}`",
        f"- 行数：{result['row_count']}",
        f"- 列数：{result['column_count']}",
        "",
        "| 字段 | 缺失 | 唯一值 | 推断类型 | 数值范围/均值 |",
        "|---|---:|---:|---|---|",
    ]
    for name, item in result["columns"].items():
        types = ", ".join(f"{key}:{value}" for key, value in item["inferred_types"].items()) or "-"
        numeric = item.get("numeric")
        numeric_text = "-" if not numeric else (
            f"{numeric['min']:.6g} ~ {numeric['max']:.6g} / {numeric['mean']:.6g}"
        )
        lines.append(
            f"| {name} | {item['missing_count']} | {item['unique_count']} | {types} | {numeric_text} |"
        )
    return "\n".join(lines) + "\n"


def main() -> None:
    """执行画像并原子意义上先创建目录、再分别写两个确定输出。"""
    args = parse_args()
    input_path = Path(args.input)
    json_output = Path(args.json_output)
    markdown_output = Path(args.markdown_output)
    records = load_records(input_path)
    result = profile(records, input_path)
    json_output.parent.mkdir(parents=True, exist_ok=True)
    markdown_output.parent.mkdir(parents=True, exist_ok=True)
    json_output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    markdown_output.write_text(render_markdown(result), encoding="utf-8")
    print(json.dumps({
        "row_count": result["row_count"],
        "column_count": result["column_count"],
        "json_output": str(json_output),
        "markdown_output": str(markdown_output),
    }, ensure_ascii=False))


if __name__ == "__main__":
    main()
